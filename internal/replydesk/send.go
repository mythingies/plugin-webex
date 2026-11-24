package replydesk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mythingies/plugin-webex/internal/safetext"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// Send rate limits.
const (
	sendMinInterval = 2 * time.Second
	sendWindow      = time.Hour
	sendMaxInWindow = 30
	// recheckPageSize is how many of the room's newest messages the pre-send
	// re-check lists.
	recheckPageSize = 20
)

// codeRecheckFailed is returned (502) when the pre-send re-check could not
// reach Webex; nothing was sent.
const codeRecheckFailed = "recheck_failed"

// sendLock returns the lock that serialises sends to one room.
func (s *Service) sendLock(roomID string) *sync.Mutex {
	s.sendLockMu.Lock()
	defer s.sendLockMu.Unlock()
	l, ok := s.sendLocks[roomID]
	if !ok {
		l = &sync.Mutex{}
		s.sendLocks[roomID] = l
	}
	return l
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sendBlocked reports whether st forbids sending. Drafting does not: the
// user may send their own text while a (re)draft runs, and the finished
// draft is then discarded.
func sendBlocked(st State) bool {
	switch st {
	case StateSent, StateDismissed, StateSending:
		return true
	}
	return false
}

func notPending(st State) *APIError {
	detail := "this conversation is not waiting for a reply"
	if st != "" {
		detail = "this conversation is " + string(st)
	}
	return &APIError{Code: CodeNotPending, Detail: detail}
}

// Send runs the pre-send checks in order and sends. The target room and
// thread come from the scan, never from the request. The POST is never
// retried: a failure leaves the room send_unknown.
func (s *Service) Send(_ context.Context, req SendRequest) (string, int, *APIError) {
	lk := s.sendLock(req.RoomID)
	lk.Lock()
	defer lk.Unlock()

	// a. The room must be pending and in a sendable state.
	s.mu.Lock()
	if apiErr := s.unavailableLocked(); apiErr != nil {
		s.mu.Unlock()
		return "", http.StatusConflict, apiErr
	}
	pp, ok := s.pending[req.RoomID]
	var st State
	if ok {
		e, _, _ := s.effectiveLocked(pp)
		st = e.state
	}
	var p Pending
	if ok {
		p = *pp
	}
	s.mu.Unlock()
	if !ok {
		return "", http.StatusConflict, notPending("")
	}
	if sendBlocked(st) {
		return "", http.StatusConflict, notPending(st)
	}
	// The reply must answer the burst the user was shown, not one that grew
	// since their page last loaded.
	if req.HeadID == "" {
		return "", http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "head_id is required"}
	}
	if req.HeadID != p.HeadID {
		return "", http.StatusConflict, &APIError{Code: CodeNewMessages, Detail: "new messages arrived since you opened this conversation; read them before replying"}
	}

	// b-f. Text checks.
	if strings.TrimSpace(req.Text) == "" {
		return "", http.StatusBadRequest, &APIError{Code: CodeEmpty, Detail: "the reply is empty"}
	}
	if sha256Hex(req.Text) != strings.ToLower(strings.TrimSpace(req.TextSHA256)) {
		return "", http.StatusBadRequest, &APIError{Code: CodeHashMismatch, Detail: "the text does not match the confirmed hash"}
	}
	if strings.Contains(req.Text, FillMarker) {
		return "", http.StatusBadRequest, &APIError{Code: CodeFillRemaining, Detail: "replace every [[FILL: ...]] placeholder first"}
	}
	if len(req.Text) > safetext.MaxMessageLen {
		return "", http.StatusBadRequest, &APIError{Code: CodeTooLong, Detail: fmt.Sprintf("the reply is %d bytes, over the %d byte limit", len(req.Text), safetext.MaxMessageLen)}
	}
	sanitized := safetext.Sanitize(req.Text)
	if sanitized != req.Text && !req.AckSanitized {
		return "", http.StatusConflict, &APIError{Code: CodeSanitized, Detail: "sanitizing changed the text; confirm the sanitized version", SanitizedText: sanitized}
	}

	// g. Rate limit; the slot is given back if the send does not happen.
	slot, ok := s.reserveSend()
	if !ok {
		return "", http.StatusTooManyRequests, &APIError{Code: CodeRateLimited, Detail: "at most one reply every 2 seconds and 30 an hour"}
	}

	// h. Live re-check against Webex (GETs only).
	if status, apiErr := s.recheck(&p); apiErr != nil {
		s.releaseSend(slot)
		return "", status, apiErr
	}

	// i. Record sending durably, then post exactly once.
	s.mu.Lock()
	cur, ok := s.pending[req.RoomID]
	if !ok || cur.HeadID != p.HeadID {
		s.mu.Unlock()
		s.releaseSend(slot)
		return "", http.StatusConflict, &APIError{Code: CodeNewMessages, Detail: "the conversation changed during the check; rescan"}
	}
	if apiErr := s.unavailableLocked(); apiErr != nil {
		s.mu.Unlock()
		s.releaseSend(slot)
		return "", http.StatusConflict, apiErr
	}
	e, r, has := s.effectiveLocked(cur)
	if sendBlocked(e.state) {
		s.mu.Unlock()
		s.releaseSend(slot)
		return "", http.StatusConflict, notPending(e.state)
	}
	var draft *Draft
	if has {
		draft = r.Draft
	}
	// While sending is set, Draft, Dismiss and Snooze refuse the room and a
	// finishing draft is held back (see recordDraft), so nothing overwrites
	// the records below while s.mu is released for the synchronous writes.
	s.sending[p.RoomID] = true
	s.mu.Unlock()

	// restore is the room as it was before this send, for the paths where
	// nothing went out.
	restore := Record{RoomID: p.RoomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated, State: StatePending}
	if has {
		restore = r
	}
	base := Record{RoomID: p.RoomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated, Draft: draft}
	sendingRec := base
	sendingRec.State = StateSending
	if err := s.putSync(sendingRec); err != nil {
		// Without the sending record on disk a crash after the POST would
		// hide that a reply may have gone out, so nothing is sent.
		s.restoreUnsent(restore, "replydesk: restoring state after a failed save")
		s.releaseSend(slot)
		switch {
		case errors.Is(err, ErrLockLost):
			return "", http.StatusConflict, lockLostErr()
		case errors.Is(err, ErrPurged):
			return "", http.StatusConflict, signedOutErr()
		}
		return "", http.StatusInternalServerError, &APIError{Code: codeInternal, Detail: shortErr("could not save Reply Desk state, so nothing was sent: ", err)}
	}

	msg, err := s.api.SendMessage(p.RoomID, "", "", p.ParentID, sanitized, "")
	if err != nil && s.noteAuthErr(err) {
		// The token provider refused before any request was made (the client
		// fetches the token first), so nothing went out: put the room back.
		s.restoreUnsent(restore, "replydesk: restoring state after sign-out")
		s.releaseSend(slot)
		return "", http.StatusConflict, signedOutErr()
	}
	if err != nil || msg == nil {
		if err == nil {
			err = errors.New("webex returned no message")
		}
		slog.Warn("replydesk: send failed; not retrying", "room_id", p.RoomID, "err", err)
		apiErr := &APIError{Code: CodeSendUnknown, Detail: shortErr("the send may or may not have gone through; check Webex before sending again: ", err)}
		unknown := base
		unknown.State = StateSendUnknown
		unknown.Error = apiErr.Detail
		_ = s.putSync(unknown) // a failure is surfaced in Status; the sending record already covers a crash
		s.doneSending(p.RoomID)
		return "", http.StatusBadGateway, apiErr
	}

	sum := sha256Hex(sanitized)
	sentDraft := &Draft{HeadID: p.HeadID, CreatedAt: s.now().UTC()}
	if draft != nil {
		c := *draft
		sentDraft = &c
		sentDraft.HeadID = p.HeadID
	}
	sentDraft.Text = sanitized
	sent := base
	sent.State = StateSent
	sent.Draft = sentDraft
	sent.SentMessageID = msg.ID
	sent.SentSHA256 = sum
	_ = s.putSync(sent) // a failure is surfaced in Status; the record is kept in memory
	s.doneSending(p.RoomID)
	slog.Info("audit", "tool", "replydesk", "action", "send", "room_id", p.RoomID, "message_id", msg.ID, "bytes", len(sanitized), "sha256", sum)
	return msg.ID, http.StatusOK, nil
}

// doneSending clears the in-flight send mark for a room after a send that
// went out, or may have. A draft that finished meanwhile is discarded.
func (s *Service) doneSending(roomID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sending, roomID)
	delete(s.heldDrafts, roomID)
}

// restoreUnsent puts a room back as it was before a send that did not go
// out and clears its sending mark. A draft that finished during the send is
// then applied as if it had finished now, so it is not lost and the room
// does not show "drafting was interrupted".
func (s *Service) restoreUnsent(restore Record, logMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Put(restore); err != nil && !errors.Is(err, ErrLockLost) {
		slog.Warn(logMsg, "err", err)
	}
	delete(s.sending, restore.RoomID)
	if o, ok := s.heldDrafts[restore.RoomID]; ok {
		delete(s.heldDrafts, restore.RoomID)
		s.recordDraftLocked(&o.p, o.draft, o.errMsg)
	}
}

// reserveSend takes a send slot if the rate limit allows one.
func (s *Service) reserveSend() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	kept := s.sendTimes[:0]
	for _, t := range s.sendTimes {
		if now.Sub(t) < sendWindow {
			kept = append(kept, t)
		}
	}
	s.sendTimes = kept
	if n := len(kept); n > 0 && now.Sub(kept[n-1]) < sendMinInterval {
		return time.Time{}, false
	}
	if len(kept) >= sendMaxInWindow {
		return time.Time{}, false
	}
	s.sendTimes = append(s.sendTimes, now)
	return now, true
}

// releaseSend gives back a slot taken by reserveSend.
func (s *Service) releaseSend(slot time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sendTimes) - 1; i >= 0; i-- {
		if s.sendTimes[i].Equal(slot) {
			s.sendTimes = slices.Delete(s.sendTimes, i, i+1)
			return
		}
	}
}

// recheck confirms with Webex that p is still unanswered and unchanged.
func (s *Service) recheck(p *Pending) (int, *APIError) {
	head, err := s.api.GetMessage(p.HeadID)
	if err != nil {
		if errors.Is(err, webex.ErrNotFound) {
			return http.StatusConflict, &APIError{Code: CodeAskDeleted, Detail: "the message you are answering was deleted"}
		}
		if s.noteAuthErr(err) {
			return http.StatusConflict, signedOutErr()
		}
		return recheckErr(err)
	}
	if head == nil {
		return http.StatusConflict, &APIError{Code: CodeAskDeleted, Detail: "the message you are answering was deleted"}
	}
	if head.Updated != p.HeadUpdated {
		return http.StatusConflict, &APIError{Code: CodeAskEdited, Detail: "the message you are answering was edited since the scan"}
	}

	msgs, err := s.api.ListMessages(webex.ListMessagesOptions{RoomID: p.RoomID, Max: recheckPageSize})
	if err != nil {
		if s.noteAuthErr(err) {
			return http.StatusConflict, signedOutErr()
		}
		return recheckErr(err)
	}
	headIdx := slices.IndexFunc(msgs, func(m webex.Message) bool { return m.ID == p.HeadID })
	fromMe, fromThem := false, false
	for i := range msgs {
		m := &msgs[i]
		if m.ID == p.HeadID {
			continue
		}
		newer := headIdx >= 0 && i < headIdx
		if t, ok := parseTime(m.Created); ok && t.After(p.HeadCreated) {
			newer = true
		}
		if !newer {
			continue
		}
		if m.PersonID == s.me {
			fromMe = true
		} else {
			fromThem = true
		}
	}
	switch {
	case fromMe:
		return http.StatusConflict, &APIError{Code: CodeAlreadyReplied, Detail: "you already replied after these messages"}
	case fromThem:
		return http.StatusConflict, &APIError{Code: CodeNewMessages, Detail: "new messages arrived since the scan; rescan"}
	}
	return 0, nil
}

func recheckErr(err error) (int, *APIError) {
	var rl *webex.RateLimitError
	if errors.As(err, &rl) {
		return http.StatusTooManyRequests, &APIError{Code: CodeRateLimited, Detail: "Webex is rate limiting; try again shortly"}
	}
	slog.Warn("replydesk: pre-send check failed; not sending", "err", err)
	return http.StatusBadGateway, &APIError{Code: codeRecheckFailed, Detail: shortErr("could not re-check the conversation with Webex, so nothing was sent: ", err)}
}

// ---- dismiss and snooze ----

// Dismiss marks the room's current burst as needing no reply.
func (s *Service) Dismiss(roomID, reason string) *APIError {
	if !validDismissReasons[reason] {
		return &APIError{Code: codeBadRequest, Detail: "unknown dismiss reason"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e, r, has, apiErr := s.closableLocked(roomID)
	if apiErr != nil {
		return apiErr
	}
	if e.state == StateSent || e.state == StateSending {
		return notPending(e.state)
	}
	rec := Record{RoomID: roomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated, State: StateDismissed, DismissReason: reason}
	if has {
		rec.Draft = r.Draft
	}
	s.putLocked(rec)
	return nil
}

// Snooze hides the room's current burst for hours (1..168).
func (s *Service) Snooze(roomID string, hours int) *APIError {
	if hours < minSnoozeHours || hours > maxSnoozeHours {
		return &APIError{Code: codeBadRequest, Detail: fmt.Sprintf("hours must be %d..%d", minSnoozeHours, maxSnoozeHours)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e, r, has, apiErr := s.closableLocked(roomID)
	if apiErr != nil {
		return apiErr
	}
	if e.state == StateSent || e.state == StateSending {
		return notPending(e.state)
	}
	rec := Record{RoomID: roomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated, State: StateSnoozed, SnoozeUntil: s.now().Add(time.Duration(hours) * time.Hour).UTC()}
	if has {
		rec.Draft = r.Draft
	}
	s.putLocked(rec)
	return nil
}

// closableLocked looks up a room for Dismiss or Snooze: signed out, unknown,
// and mid-send rooms are refused. s.mu must be held.
func (s *Service) closableLocked(roomID string) (*Pending, effective, Record, bool, *APIError) {
	if apiErr := s.unavailableLocked(); apiErr != nil {
		return nil, effective{}, Record{}, false, apiErr
	}
	p, ok := s.pending[roomID]
	if !ok {
		return nil, effective{}, Record{}, false, notPending("")
	}
	if s.sending[roomID] {
		return nil, effective{}, Record{}, false, notPending(StateSending)
	}
	e, r, has := s.effectiveLocked(p)
	return p, e, r, has, nil
}
