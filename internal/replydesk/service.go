package replydesk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
	"github.com/mythingies/plugin-webex/internal/safetext"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// Scan limits.
const (
	// scanConcurrency is how many rooms are fetched at once.
	scanConcurrency = 4
	// scanRoomLimit caps the DM rooms listed per scan.
	scanRoomLimit = 200
	// scanPageSize is the GET /messages page size during a scan.
	scanPageSize = 50
	// scanMaxMessages caps the history fetched per room during a scan.
	scanMaxMessages = 200
	// scanMaxAge stops paging a room's history at messages this old.
	scanMaxAge = 90 * 24 * time.Hour
	// scanTimeout bounds one background scan.
	scanTimeout = 10 * time.Minute
)

// Service limits.
const (
	defaultPreDraft = 5
	// maxDraftsInFlight caps concurrent drafter runs, background and manual.
	maxDraftsInFlight = 2
	// draftTimeout bounds one background draft, grounding included.
	draftTimeout = 5 * time.Minute
	// recordRetention is the minimum time an untouched record is kept. With a
	// long lookback it is extended (see retentionFor), so a dismissed room
	// can't outlive its record and come back.
	recordRetention = 30 * 24 * time.Hour
	// retentionPastLookback is how far past the lookback records are kept.
	retentionPastLookback = 7 * 24 * time.Hour
	// maxErrorLen caps error strings shown to the user.
	maxErrorLen = 300
)

// User-facing messages for states that are not errors of one request.
const (
	msgDraftInterrupted = "drafting was interrupted"
	msgSignedOut        = "You signed out in another window - restart webex-mcp --review"
	msgLockLost         = "Reply Desk was opened in another window; this window no longer saves changes - close it"
)

// retentionFor is how long untouched records are kept for a lookback: at
// least recordRetention, and a week past the lookback, so a record outlives
// every burst a scan can still list.
func retentionFor(lookback time.Duration) time.Duration {
	return max(recordRetention, lookback+retentionPastLookback)
}

// Scan lists the user's DM rooms active within lookback and returns every
// pending burst, newest head first. It issues only GETs.
//
// A *webex.RateLimitError aborts the scan: the results so far are returned
// with that error. Any other per-room error skips the room; the scan goes
// on and returns the partial results with the joined errors.
func Scan(ctx context.Context, api API, me string, lookback time.Duration, now time.Time) ([]Pending, error) {
	res := scanRooms(ctx, api, me, lookback, now)
	return res.pending, res.err
}

// scanResult is a scan's outcome, with the bookkeeping the Service needs to
// keep earlier results for rooms this scan could not check.
type scanResult struct {
	pending []Pending
	// done holds the rooms checked to completion (pending or answered).
	done map[string]bool
	// aborted is true when the scan stopped early (rate limit, context, or
	// a failed room listing), so rooms not in done were never looked at.
	aborted bool
	// failed holds the rooms skipped because of an error.
	failed map[string]bool
	err    error
}

func scanRooms(ctx context.Context, api API, me string, lookback time.Duration, now time.Time) scanResult {
	res := scanResult{done: make(map[string]bool), failed: make(map[string]bool)}
	rooms, err := api.ListDirectRooms(now.Add(-lookback), scanRoomLimit)
	var listErr error
	if err != nil {
		var rl *webex.RateLimitError
		if errors.As(err, &rl) || len(rooms) == 0 {
			res.aborted = true
			res.err = fmt.Errorf("listing direct rooms: %w", err)
			return res
		}
		// Some rooms came back before the error: scan those.
		res.aborted = true
		listErr = fmt.Errorf("listing direct rooms: %w", err)
	}

	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type roomOut struct {
		p    *Pending
		err  error
		done bool
	}
	outs := make([]roomOut, len(rooms))
	var (
		wg    sync.WaitGroup
		rlMu  sync.Mutex
		rlErr error
		sem   = make(chan struct{}, scanConcurrency)
	)
launch:
	for i := range rooms {
		if scanCtx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-scanCtx.Done():
			break launch
		}
		wg.Add(1)
		go func(i int, room webex.Space) {
			defer wg.Done()
			defer func() { <-sem }()
			if scanCtx.Err() != nil {
				return
			}
			p, err := scanRoom(scanCtx, api, me, room, lookback, now)
			if err != nil {
				var rl *webex.RateLimitError
				if errors.As(err, &rl) {
					rlMu.Lock()
					if rlErr == nil {
						rlErr = err
					}
					rlMu.Unlock()
					cancel()
					if o, ok := api.(abortObserver); ok {
						o.scanAborted()
					}
					return
				}
				if scanCtx.Err() != nil {
					return // cancelled; neither done nor failed
				}
				outs[i] = roomOut{err: fmt.Errorf("room %s: %w", room.ID, err)}
				return
			}
			outs[i] = roomOut{p: p, done: true}
		}(i, rooms[i])
	}
	wg.Wait()

	var errs []error
	if listErr != nil {
		errs = append(errs, listErr)
	}
	for i := range outs {
		o := &outs[i]
		switch {
		case o.done:
			res.done[rooms[i].ID] = true
			if o.p != nil {
				res.pending = append(res.pending, *o.p)
			}
		case o.err != nil:
			res.failed[rooms[i].ID] = true
			errs = append(errs, o.err)
		}
	}
	sortPending(res.pending)

	switch {
	case rlErr != nil:
		res.aborted = true
		res.err = rlErr
	case ctx.Err() != nil:
		res.aborted = true
		res.err = errors.Join(append([]error{ctx.Err()}, errs...)...)
	default:
		res.err = errors.Join(errs...)
	}
	return res
}

// abortObserver is implemented by test fakes that need to know when a rate
// limit has cancelled a scan. *webex.Client does not implement it.
type abortObserver interface {
	scanAborted()
}

// scanRoom fetches enough of one room's history to detect its burst and
// returns the pending item, or nil when the room needs no reply.
func scanRoom(ctx context.Context, api API, me string, room webex.Space, lookback time.Duration, now time.Time) (*Pending, error) {
	if room.ID == "" {
		return nil, nil
	}
	cutoff := now.Add(-scanMaxAge)
	var msgs []webex.Message
	before := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := api.ListMessages(webex.ListMessagesOptions{RoomID: room.ID, BeforeMessage: before, Max: scanPageSize})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		first := len(msgs) == 0
		msgs = append(msgs, page...)
		if first {
			head := msgs[0]
			if head.PersonID == me || IsBot(head.PersonEmail) {
				return nil, nil // answered, or a bot
			}
			if t, ok := parseTime(head.Created); ok && now.Sub(t) > lookback {
				return nil, nil // newest message is outside the lookback
			}
		}
		if !needMoreHistory(msgs, me) || len(msgs) >= scanMaxMessages {
			break
		}
		oldest := msgs[len(msgs)-1]
		if t, ok := parseTime(oldest.Created); ok && !t.After(cutoff) {
			break
		}
		if oldest.ID == "" || oldest.ID == before {
			break // the API is not paging; don't loop
		}
		before = oldest.ID
	}
	if len(msgs) > scanMaxMessages {
		msgs = msgs[:scanMaxMessages]
	}
	return Detect(me, room, msgs, now, lookback), nil
}

// needMoreHistory reports whether newestFirst lacks the user's last reply or
// the ContextMessages messages older than it.
func needMoreHistory(newestFirst []webex.Message, me string) bool {
	idx := slices.IndexFunc(newestFirst, func(m webex.Message) bool { return m.PersonID == me })
	if idx < 0 {
		return true
	}
	return len(newestFirst)-1-idx < ContextMessages
}

// sortPending orders by HeadCreated, newest first, then room ID.
func sortPending(ps []Pending) {
	slices.SortStableFunc(ps, func(a, b Pending) int {
		if c := b.HeadCreated.Compare(a.HeadCreated); c != 0 {
			return c
		}
		return strings.Compare(a.RoomID, b.RoomID)
	})
}

// Config configures a Service.
type Config struct {
	API       API
	StorePath string // "" keeps state in memory only
	Drafter   Drafter
	Lookback  time.Duration    // default DefaultLookback
	Now       func() time.Time // default time.Now
	PreDraft  int              // drafts started in the background after a scan; default 5
}

// Service holds the scan results, drafts, and send logic behind the UI.
//
// Locking: mu guards the in-memory state below it. It is held while reading
// and writing store records with Store.Put (which updates memory and queues
// the write), so state decisions are atomic, but never across Webex calls,
// drafter runs, or Store.PutSync. sendLocks serialise sends per room and are
// always taken before mu.
type Service struct {
	api      API
	drafter  Drafter
	store    *Store
	me       string
	meName   string
	lookback time.Duration
	now      func() time.Time
	preDraft int
	// signedOut reports whether the stored state was purged by --logout or
	// --switch in another process. It is store.Disabled; tests replace it.
	signedOut func() bool
	// lockLost reports whether another --review took over the store, so
	// this process no longer saves. It is store.LockLost; tests replace it.
	lockLost func() bool
	// authSignedOut is set once a Webex call failed with auth.ErrSignedOut:
	// --logout or --switch ran in another process. Guarded by mu.
	authSignedOut bool

	draftSem     chan struct{}
	draftTimeout time.Duration
	wg           sync.WaitGroup // background scans and drafts

	// rootCtx is the parent of every background scan and draft; Close
	// cancels it.
	rootCtx    context.Context
	rootCancel context.CancelFunc
	closeOnce  sync.Once
	closeErr   error

	sendLockMu sync.Mutex
	sendLocks  map[string]*sync.Mutex

	mu       sync.Mutex
	closed   bool // Close was called; no new background work starts
	pending  map[string]*Pending
	scanning bool
	lastScan time.Time
	scanErr  string
	storeErr string
	// drafting holds the rooms with a draft queued or running, mapped to
	// the regenerate instruction it was started with. A queued draft (one
	// still waiting for a drafter slot) counts: the room shows as drafting
	// from the moment the user asks.
	drafting map[string]string
	sending  map[string]bool // rooms with a send in progress
	// heldDrafts holds the outcome of a draft that finished while its room
	// was sending. A send that does not go out (its record could not be
	// saved, or the user was signed out) applies it when it restores the
	// room; a send that went out, or may have, discards it.
	heldDrafts map[string]draftOutcome
	sendTimes  []time.Time // recent sends, for the rate limit
}

// draftOutcome is a finished draft run: the room as drafted, and the draft
// or the error to show.
type draftOutcome struct {
	p      Pending
	draft  *Draft
	errMsg string
}

// NewService resolves the user (GetMe), opens the store bound to them, and
// returns a Service. Call Close when done with it.
func NewService(cfg Config) (*Service, error) {
	if cfg.API == nil {
		return nil, errors.New("replydesk: Config.API is required")
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultLookback
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PreDraft == 0 {
		cfg.PreDraft = defaultPreDraft
	}
	if cfg.PreDraft < 0 {
		cfg.PreDraft = 0
	}
	if cfg.Drafter == nil {
		cfg.Drafter = offDrafter{}
	}

	me, err := cfg.API.GetMe()
	if err != nil {
		return nil, fmt.Errorf("identifying you (people/me): %w", err)
	}
	if me == nil || me.ID == "" {
		return nil, errors.New("identifying you (people/me): no person ID returned")
	}
	store, err := OpenStore(cfg.StorePath, me.ID)
	if err != nil {
		return nil, fmt.Errorf("opening Reply Desk state: %w", err)
	}
	store.now = cfg.Now

	name := me.DisplayName
	if name == "" {
		name = me.NickName
	}
	rootCtx, rootCancel := context.WithCancel(context.Background()) // #nosec G118 -- rootCancel is kept on the Service and called by Close
	s := &Service{
		api:          cfg.API,
		drafter:      cfg.Drafter,
		store:        store,
		me:           me.ID,
		meName:       name,
		lookback:     cfg.Lookback,
		now:          cfg.Now,
		preDraft:     cfg.PreDraft,
		signedOut:    store.Disabled,
		lockLost:     store.LockLost,
		draftSem:     make(chan struct{}, maxDraftsInFlight),
		draftTimeout: draftTimeout,
		rootCtx:      rootCtx,
		rootCancel:   rootCancel,
		sendLocks:    make(map[string]*sync.Mutex),
		pending:      make(map[string]*Pending),
		drafting:     make(map[string]string),
		sending:      make(map[string]bool),
		heldDrafts:   make(map[string]draftOutcome),
	}
	if n, err := store.Prune(cfg.Now().Add(-retentionFor(cfg.Lookback))); err != nil {
		s.noteStoreErr(err)
	} else if n > 0 {
		slog.Info("replydesk: pruned old records", "count", n)
	}
	return s, nil
}

var _ Backend = (*Service)(nil)

// wait blocks until background scans and drafts finish. Tests use it.
func (s *Service) wait() { s.wg.Wait() }

// Close cancels background scans and drafts (killing any claude subprocess)
// and waits for them, up to ctx's deadline, then closes the store. It is
// safe to call more than once; later calls return the first call's result.
func (s *Service) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true // no wg.Add after this, so the Wait below is safe
		s.mu.Unlock()
		s.rootCancel()

		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		var waitErr error
		select {
		case <-done:
		case <-ctx.Done():
			waitErr = fmt.Errorf("waiting for background drafts and scans: %w", ctx.Err())
			slog.Warn("replydesk: closing with background work still running", "err", ctx.Err())
		}
		var storeErr error
		if err := s.store.Close(); err != nil {
			storeErr = fmt.Errorf("closing Reply Desk state: %w", err)
		}
		s.closeErr = errors.Join(waitErr, storeErr)
	})
	return s.closeErr
}

// isSignedOutLocked reports whether --logout or --switch purged the state
// in another process. s.mu must be held.
func (s *Service) isSignedOutLocked() bool {
	return s.authSignedOut || (s.signedOut != nil && s.signedOut())
}

// noteAuthErr records a sign-out detected by the token provider. It reports
// whether err was one.
func (s *Service) noteAuthErr(err error) bool {
	if !errors.Is(err, auth.ErrSignedOut) {
		return false
	}
	s.mu.Lock()
	s.authSignedOut = true
	s.mu.Unlock()
	return true
}

// isLockLostLocked reports whether another --review took over the store.
// s.mu must be held.
func (s *Service) isLockLostLocked() bool {
	return s.lockLost != nil && s.lockLost()
}

// unavailableLocked is the error every action returns once this process
// must stop: signed out elsewhere, or the store taken over. s.mu must be
// held.
func (s *Service) unavailableLocked() *APIError {
	switch {
	case s.isSignedOutLocked():
		return signedOutErr()
	case s.isLockLostLocked():
		return lockLostErr()
	}
	return nil
}

// SignedOut reports whether --logout or --switch ran in another process
// while this one was open. The server refuses items and scans then.
func (s *Service) SignedOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isSignedOutLocked()
}

// signedOutErr is returned by every state-changing action once the state
// was purged by another process.
func signedOutErr() *APIError {
	return &APIError{Code: CodeSignedOut, Detail: msgSignedOut}
}

// lockLostErr is returned by every action once another --review took over
// the store.
func lockLostErr() *APIError {
	return &APIError{Code: CodeLockLost, Detail: msgLockLost}
}

// ---- effective state ----

// effective is a room's state as the UI sees it.
type effective struct {
	state       State
	err         string
	draft       *Draft
	stale       bool
	snoozeUntil time.Time
}

// sameBurst reports whether a record written for a different head still
// applies to p: the record closed the room (dismissed, sent, or snoozed) and
// p's head is not newer than the record's. That happens when the newest
// message is deleted and an older one becomes the head; only a genuinely
// newer message re-opens the room.
func sameBurst(p *Pending, r Record) bool {
	switch r.State {
	case StateDismissed, StateSent, StateSnoozed:
	default:
		return false
	}
	return !r.HeadCreated.IsZero() && !p.HeadCreated.After(r.HeadCreated)
}

// effectiveState derives the state shown for p from its stored record.
// drafting and sending say whether this process has that operation in
// flight for the room.
func effectiveState(p *Pending, r Record, has, drafting, sending bool, now time.Time) effective {
	var e effective
	switch {
	case !has:
		e.state = StatePending
		if drafting {
			e.state = StateDrafting
		}
	case r.HeadID != p.HeadID && !sameBurst(p, r):
		// The record is for an older head: the room is open again. A draft
		// written for that head is still shown, marked stale.
		e.state = StatePending
		if drafting {
			e.state = StateDrafting
		}
		e.draft = r.Draft
	default:
		e.draft = r.Draft
		e.err = r.Error
		switch r.State {
		case StateSnoozed:
			if now.Before(r.SnoozeUntil) {
				e.state = StateSnoozed
				e.snoozeUntil = r.SnoozeUntil
			} else {
				e.state = StatePending
				e.err = ""
				if drafting {
					e.state = StateDrafting
				}
			}
		case StateDrafting:
			if drafting {
				e.state = StateDrafting
			} else {
				e.state = StateDraftFailed
				e.err = msgDraftInterrupted
			}
		case StateSending:
			if sending {
				e.state = StateSending
			} else {
				e.state = StateSendUnknown
				if e.err == "" {
					e.err = "the send was interrupted; check Webex before sending again"
				}
			}
		case "":
			e.state = StatePending
		default:
			e.state = r.State
		}
	}
	e.stale = e.draft != nil && e.draft.HeadID != p.HeadID
	return e
}

// effectiveLocked returns the room's effective state and its stored record.
// s.mu must be held.
func (s *Service) effectiveLocked(p *Pending) (effective, Record, bool) {
	r, has := s.store.Get(p.RoomID)
	_, drafting := s.drafting[p.RoomID]
	return effectiveState(p, r, has, drafting, s.sending[p.RoomID], s.now()), r, has
}

// putLocked saves a record with Store.Put, logging and surfacing a failure.
// The store keeps the record in memory even when the write fails. s.mu must
// be held.
func (s *Service) putLocked(r Record) {
	if err := s.store.Put(r); err != nil {
		s.noteStoreErrLocked(err)
		return
	}
	s.storeErr = ""
}

// putSync saves a record durably with Store.PutSync, surfacing a failure.
// s.mu must not be held: the write syncs to disk (and runs icacls on
// Windows).
func (s *Service) putSync(r Record) error {
	err := s.store.PutSync(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.noteStoreErrLocked(err)
	} else {
		s.storeErr = ""
	}
	return err
}

func (s *Service) noteStoreErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteStoreErrLocked(err)
}

func (s *Service) noteStoreErrLocked(err error) {
	if !errors.Is(err, ErrLockLost) { // the store logged the takeover once
		slog.Warn("replydesk: saving state failed", "err", err)
	}
	s.storeErr = shortErr("saving Reply Desk state failed: ", err)
}

// shortErr is prefix + err, capped at maxErrorLen bytes.
func shortErr(prefix string, err error) string {
	return capBytes(prefix+err.Error(), maxErrorLen)
}

// ---- views ----

// stateGroup orders the inbox: rooms needing action, then snoozed, then done.
func stateGroup(st State) int {
	switch st {
	case StateSnoozed:
		return 1
	case StateSent, StateDismissed:
		return 2
	default:
		return 0
	}
}

// Items lists one item per pending room. Sent and dismissed rooms are hidden
// unless all is set. Records for rooms the last scan did not list (answered
// since, or outside the lookback) are never shown. Once signed out
// elsewhere the list is empty: those are the signed-out account's DMs.
func (s *Service) Items(all bool) []ItemView {
	s.mu.Lock()
	if s.isSignedOutLocked() {
		s.mu.Unlock()
		return []ItemView{}
	}
	type row struct {
		p *Pending
		e effective
	}
	rows := make([]row, 0, len(s.pending))
	for _, p := range s.pending {
		e, _, _ := s.effectiveLocked(p)
		if !all && (e.state == StateSent || e.state == StateDismissed) {
			continue
		}
		rows = append(rows, row{p, e})
	}
	s.mu.Unlock()

	slices.SortFunc(rows, func(a, b row) int {
		if c := stateGroup(a.e.state) - stateGroup(b.e.state); c != 0 {
			return c
		}
		if c := b.p.HeadCreated.Compare(a.p.HeadCreated); c != 0 {
			return c
		}
		return strings.Compare(a.p.RoomID, b.p.RoomID)
	})
	out := make([]ItemView, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.itemView(r.p, r.e))
	}
	return out
}

func (s *Service) itemView(p *Pending, e effective) ItemView {
	v := ItemView{
		RoomID:      p.RoomID,
		HeadID:      p.HeadID,
		Title:       p.RoomTitle,
		State:       e.state,
		Since:       fmtTime(p.Since),
		HeadCreated: fmtTime(p.HeadCreated),
		Reason:      p.Reason,
		Flags:       append([]string{}, p.Flags...),
		InThread:    p.ParentID != "",
		Burst:       s.msgViews(p, p.Burst),
		Context:     s.msgViews(p, p.Context),
		DraftStale:  e.stale,
		Error:       e.err,
	}
	if e.draft != nil {
		v.Draft = draftView(e.draft)
	}
	if e.state == StateSnoozed && !e.snoozeUntil.IsZero() {
		v.SnoozeUntil = fmtTime(e.snoozeUntil)
	}
	return v
}

func (s *Service) msgViews(p *Pending, msgs []webex.Message) []MsgView {
	out := make([]MsgView, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		fromMe := m.PersonID == s.me
		author := p.RoomTitle
		if fromMe {
			author = "You"
		}
		out = append(out, MsgView{
			ID:        m.ID,
			FromMe:    fromMe,
			Author:    author,
			Created:   fmtStamp(m.Created),
			Text:      m.Text,
			InThread:  m.ParentID != "",
			Files:     len(m.Files),
			Card:      len(m.Attachments) > 0,
			VoiceClip: m.IsVoiceClip,
			Edited:    m.Updated != "",
		})
	}
	return out
}

func draftView(d *Draft) *DraftView {
	v := &DraftView{
		Text:             d.Text,
		Summary:          d.Summary,
		Asks:             append([]string{}, d.Asks...),
		MissingInfo:      append([]string{}, d.MissingInfo...),
		NeedsReply:       d.NeedsReply,
		NeedsReplyReason: d.NeedsReplyReason,
		NoPriorAnswer:    d.NoPriorAnswer,
		Cited:            make([]CitationView, 0, len(d.Cited)),
		Instruction:      d.Instruction,
		Mode:             d.Mode,
		CreatedAt:        fmtTime(d.CreatedAt),
	}
	for _, c := range d.Cited {
		v.Cited = append(v.Cited, CitationView{Created: fmtStamp(c.Created), Text: c.Text})
	}
	return v
}

// fmtTime formats t as RFC 3339, or "" for the zero time.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// fmtStamp normalises a Webex timestamp to RFC 3339, keeping it as is when
// it does not parse.
func fmtStamp(s string) string {
	if t, ok := parseTime(s); ok {
		return fmtTime(t)
	}
	return s
}

// fmtLookback renders a lookback as "7d" for whole days, else a Go duration.
func fmtLookback(d time.Duration) string {
	const day = 24 * time.Hour
	if d > 0 && d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}
	return d.String()
}

// Status is the status bar. Drafting counts drafts queued or running, so it
// can exceed maxDraftsInFlight while drafts wait for a drafter slot.
func (s *Service) Status() StatusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	errMsg := s.scanErr
	if s.storeErr != "" {
		if errMsg != "" {
			errMsg += "; "
		}
		errMsg += s.storeErr
	}
	switch {
	case s.isSignedOutLocked():
		// Scan and save errors after a sign-out follow from it.
		errMsg = msgSignedOut
	case s.isLockLostLocked():
		// So do save errors after a takeover.
		errMsg = msgLockLost
	}
	return StatusView{
		Me:            s.meName,
		DrafterMode:   s.drafter.Mode(),
		LastScan:      fmtTime(s.lastScan),
		Scanning:      s.scanning,
		Drafting:      len(s.drafting),
		Lookback:      fmtLookback(s.lookback),
		Error:         errMsg,
		MaxMessageLen: safetext.MaxMessageLen,
	}
}

// ---- scanning ----

// Rescan starts a background scan unless one is already running, the
// Service is closed, or the user signed out elsewhere (the scan would list
// the signed-out account's DMs). After a store takeover it still scans: the
// results stay in memory, and drafting and every action are refused. The
// scan stops when ctx is cancelled or Close is called. When it finishes,
// the pending set is replaced and the newest items are drafted.
func (s *Service) Rescan(ctx context.Context) {
	s.mu.Lock()
	if s.scanning || s.closed || s.isSignedOutLocked() {
		s.mu.Unlock()
		return
	}
	s.scanning = true
	s.wg.Add(1) // under mu, so it can't race Close's Wait
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		scanCtx, cancel := context.WithTimeout(s.rootCtx, scanTimeout)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		res := scanRooms(scanCtx, s.api, s.me, s.lookback, s.now())
		s.finishScan(res)
	}()
}

func (s *Service) finishScan(res scanResult) {
	s.noteAuthErr(res.err)
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]*Pending, len(res.pending))
	for i := range res.pending {
		p := res.pending[i]
		next[p.RoomID] = &p
	}
	// Keep the previous result for rooms this scan could not check, so an
	// error doesn't silently empty the inbox.
	for id, p := range s.pending {
		if _, ok := next[id]; ok || res.done[id] {
			continue
		}
		if res.aborted || res.failed[id] {
			next[id] = p
		}
	}
	if s.isSignedOutLocked() {
		// A scan that was running when the user signed out elsewhere: do
		// not keep that account's DMs.
		next = make(map[string]*Pending)
	}
	s.pending = next
	s.scanning = false
	s.lastScan = s.now()
	s.scanErr = scanErrMessage(res)
	if res.err != nil {
		slog.Warn("replydesk: scan incomplete", "err", res.err, "failed_rooms", len(res.failed), "aborted", res.aborted)
	}
	slog.Info("replydesk: scan finished", "pending", len(res.pending))

	s.preDraftLocked()
}

// scanErrMessage is the short status-bar text for a scan's error.
func scanErrMessage(res scanResult) string {
	if res.err == nil {
		return ""
	}
	var rl *webex.RateLimitError
	if errors.As(res.err, &rl) {
		return capBytes(fmt.Sprintf("Webex rate limited the scan; results may be incomplete. Try again in %s.", rl.RetryAfter.Round(time.Second)), maxErrorLen)
	}
	if len(res.failed) > 0 && !res.aborted {
		return shortErr(fmt.Sprintf("%d conversation(s) could not be scanned: ", len(res.failed)), res.err)
	}
	return shortErr("scan incomplete: ", res.err)
}

// preDraftLocked starts background drafts for the newest pending items
// without a draft for their current head. s.mu must be held.
func (s *Service) preDraftLocked() {
	if s.preDraft <= 0 || s.drafter.Mode() == ModeOff || s.closed || s.unavailableLocked() != nil {
		return
	}
	ps := make([]*Pending, 0, len(s.pending))
	for _, p := range s.pending {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b *Pending) int {
		if c := b.HeadCreated.Compare(a.HeadCreated); c != 0 {
			return c
		}
		return strings.Compare(a.RoomID, b.RoomID)
	})
	started := 0
	for _, p := range ps {
		if started >= s.preDraft {
			break
		}
		e, r, has := s.effectiveLocked(p)
		if e.state != StatePending || (e.draft != nil && e.draft.HeadID == p.HeadID) || !hasDraftableText(p) {
			continue
		}
		s.startDraftLocked(p, r, has, e, "", "")
		started++
	}
}

// hasDraftableText reports whether any burst message has text.
func hasDraftableText(p *Pending) bool {
	for i := range p.Burst {
		if strings.TrimSpace(messageText(&p.Burst[i])) != "" {
			return true
		}
	}
	return false
}

// ---- drafting ----

// Draft starts (re)drafting one room in the background. ctx is not used for
// the draft itself, which outlives the request.
//
// While a draft for the room is queued or running, repeating a plain request
// (no instruction, and the running draft has none) is a no-op; any other
// request returns CodeBusy rather than silently dropping the instruction.
func (s *Service) Draft(_ context.Context, roomID, instruction string) *APIError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if apiErr := s.unavailableLocked(); apiErr != nil {
		return apiErr
	}
	p, ok := s.pending[roomID]
	if !ok {
		return &APIError{Code: CodeNotPending, Detail: "this conversation is not waiting for a reply"}
	}
	if s.sending[roomID] {
		return notPending(StateSending)
	}
	e, r, has := s.effectiveLocked(p)
	switch e.state {
	case StateSent, StateDismissed, StateSending:
		return &APIError{Code: CodeNotPending, Detail: "this conversation is already " + string(e.state)}
	case StateSendUnknown:
		// Regenerating would replace the send_unknown warning with a fresh
		// draft and invite a duplicate send.
		return &APIError{Code: CodeNotPending, Detail: "the last send may have gone through; check Webex, then edit and send or dismiss"}
	}
	if running, inFlight := s.drafting[roomID]; inFlight {
		if instruction == "" && running == "" {
			return nil
		}
		return &APIError{Code: CodeBusy, Detail: "a draft is already being written for this conversation; try again when it finishes"}
	}
	if s.closed {
		return &APIError{Code: CodeBusy, Detail: "Reply Desk is shutting down"}
	}
	if s.drafter.Mode() == ModeOff {
		return &APIError{Code: CodeDrafterOff, Detail: "drafting is off: the claude CLI is not available"}
	}
	prev := ""
	if has && r.Draft != nil {
		prev = r.Draft.Text
	}
	s.startDraftLocked(p, r, has, e, instruction, prev)
	return nil
}

// startDraftLocked marks the room as drafting and runs the drafter in the
// background. e is the room's effective state: a snoozed room keeps its
// snooze, and the draft is attached to it when it finishes. Any other room is
// recorded as drafting. s.mu must be held.
func (s *Service) startDraftLocked(p *Pending, r Record, has bool, e effective, instruction, previous string) {
	if s.closed {
		return // no wg.Add once Close may be waiting
	}
	s.drafting[p.RoomID] = instruction
	if e.state != StateSnoozed {
		rec := Record{RoomID: p.RoomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated, State: StateDrafting}
		if has {
			rec.Draft = r.Draft
		}
		s.putLocked(rec)
	}

	snap := *p
	s.wg.Add(1)
	go s.runDraft(snap, instruction, previous)
}

// runDraft grounds, drafts, and records the outcome for one room.
func (s *Service) runDraft(p Pending, instruction, previous string) {
	defer s.wg.Done()
	draft, errMsg := s.writeDraft(&p, instruction, previous)
	s.recordDraft(&p, draft, errMsg)
}

// writeDraft waits for a drafter slot, then grounds and drafts p. It returns
// the draft, or the error to show. Close interrupts it.
func (s *Service) writeDraft(p *Pending, instruction, previous string) (*Draft, string) {
	select {
	case s.draftSem <- struct{}{}:
	case <-s.rootCtx.Done():
		return nil, msgDraftInterrupted
	}
	defer func() { <-s.draftSem }()

	ctx, cancel := context.WithTimeout(s.rootCtx, s.draftTimeout)
	defer cancel()

	in, refs := s.buildDraftInput(ctx, p, instruction, previous)
	out, err := s.drafter.Draft(ctx, in)

	switch {
	case s.rootCtx.Err() != nil:
		return nil, msgDraftInterrupted
	case err != nil:
		errMsg := capBytes(err.Error(), maxErrorLen)
		slog.Warn("replydesk: draft failed", "room_id", p.RoomID, "err", errMsg)
		return nil, errMsg
	case out == nil:
		return nil, "drafter returned no output"
	case out.NeedsReply && strings.TrimSpace(out.Draft) == "":
		return nil, "drafter returned an empty draft"
	}
	return &Draft{
		HeadID:           p.HeadID,
		Text:             out.Draft,
		Summary:          out.Summary,
		Asks:             append([]string{}, out.Asks...),
		MissingInfo:      append([]string{}, out.MissingInfo...),
		NeedsReply:       out.NeedsReply,
		NeedsReplyReason: out.NeedsReplyReason,
		NoPriorAnswer:    out.NoPriorAnswer,
		Cited:            citations(out.CitedRefs, refs),
		Instruction:      instruction,
		Mode:             s.drafter.Mode(),
		CreatedAt:        s.now().UTC(),
	}, ""
}

// recordDraft stores a finished draft (or its failure) for p. A send in
// progress or done always wins. While the room is sending the outcome is
// held: the send applies it if it restores the room without sending, and
// discards it otherwise.
func (s *Service) recordDraft(p *Pending, draft *Draft, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.drafting, p.RoomID)
	if s.sending[p.RoomID] {
		// The send's record must not be overwritten.
		s.heldDrafts[p.RoomID] = draftOutcome{p: *p, draft: draft, errMsg: errMsg}
		return
	}
	s.recordDraftLocked(p, draft, errMsg)
}

// recordDraftLocked applies a finished draft to the room's stored record.
// s.mu must be held and the room must not be sending.
func (s *Service) recordDraftLocked(p *Pending, draft *Draft, errMsg string) {
	r, has := s.store.Get(p.RoomID)
	switch {
	case !has || (r.HeadID == p.HeadID && r.State == StateDrafting):
		// The usual case. If the room's head moved on during drafting the
		// record still names the head the draft was written for, so the UI
		// shows the draft as stale.
		rec := Record{RoomID: p.RoomID, HeadID: p.HeadID, HeadCreated: p.HeadCreated}
		if has {
			rec.Draft = r.Draft
		}
		if draft != nil {
			rec.State = StateDrafted
			rec.Draft = draft
		} else {
			rec.State = StateDraftFailed
			rec.Error = errMsg
		}
		s.putLocked(rec)
	case r.State == StateSending || r.State == StateSent || r.State == StateSendUnknown:
		// Sent, or maybe sent, meanwhile: discard the draft.
	case draft != nil:
		// The room was dismissed or snoozed meanwhile, or the draft was
		// asked for while snoozed: keep that state, but attach the draft.
		r.Draft = draft
		if r.State == StateSnoozed {
			r.Error = ""
		}
		s.putLocked(r)
	case r.State == StateSnoozed:
		// A draft asked for while snoozed failed: show why on the item.
		r.Error = errMsg
		s.putLocked(r)
	}
}

// citations maps the drafter's cited refs back to messages. A message is
// cited once even if the drafter names it under two refs.
func citations(cited []string, refs map[string]webex.Message) []Citation {
	out := []Citation{}
	seen := make(map[string]bool, len(cited))
	for _, ref := range cited {
		m, ok := refs[ref]
		if !ok {
			continue
		}
		key := "id:" + m.ID
		if m.ID == "" {
			key = "ref:" + ref
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Citation{
			MessageID: m.ID,
			Created:   m.Created,
			Text:      capBytes(refText(&m), MaxCitationLen),
		})
	}
	return out
}
