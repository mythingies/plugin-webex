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
	// recordRetention is how long an untouched record is kept.
	recordRetention = 30 * 24 * time.Hour
	// maxErrorLen caps error strings shown to the user.
	maxErrorLen = 300
)

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
		wg     sync.WaitGroup
		rlMu   sync.Mutex
		rlErr  error
		sem    = make(chan struct{}, scanConcurrency)
		launch = true
	)
	for i := range rooms {
		if scanCtx.Err() != nil {
			launch = false
			break
		}
		select {
		case sem <- struct{}{}:
		case <-scanCtx.Done():
			launch = false
		}
		if !launch {
			break
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
// and writing store records (local disk only) so state decisions are atomic,
// but never across Webex calls or drafter runs. sendLocks serialise sends per
// room and are always taken before mu.
type Service struct {
	api      API
	drafter  Drafter
	store    *Store
	me       string
	meName   string
	lookback time.Duration
	now      func() time.Time
	preDraft int

	draftSem     chan struct{}
	draftTimeout time.Duration
	wg           sync.WaitGroup // background scans and drafts

	sendLockMu sync.Mutex
	sendLocks  map[string]*sync.Mutex

	mu        sync.Mutex
	pending   map[string]*Pending
	scanning  bool
	lastScan  time.Time
	scanErr   string
	storeErr  string
	drafting  map[string]bool // rooms with a draft queued or running
	sending   map[string]bool // rooms with a send in progress
	sendTimes []time.Time     // recent sends, for the rate limit
}

// NewService resolves the user (GetMe), opens the store bound to them, and
// returns a Service.
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
	s := &Service{
		api:          cfg.API,
		drafter:      cfg.Drafter,
		store:        store,
		me:           me.ID,
		meName:       name,
		lookback:     cfg.Lookback,
		now:          cfg.Now,
		preDraft:     cfg.PreDraft,
		draftSem:     make(chan struct{}, maxDraftsInFlight),
		draftTimeout: draftTimeout,
		sendLocks:    make(map[string]*sync.Mutex),
		pending:      make(map[string]*Pending),
		drafting:     make(map[string]bool),
		sending:      make(map[string]bool),
	}
	if n, err := store.Prune(cfg.Now().Add(-recordRetention)); err != nil {
		s.noteStoreErr(err)
	} else if n > 0 {
		slog.Info("replydesk: pruned old records", "count", n)
	}
	return s, nil
}

var _ Backend = (*Service)(nil)

// wait blocks until background scans and drafts finish. Tests use it.
func (s *Service) wait() { s.wg.Wait() }

// ---- effective state ----

// effective is a room's state as the UI sees it.
type effective struct {
	state       State
	err         string
	draft       *Draft
	stale       bool
	snoozeUntil time.Time
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
	case r.HeadID != p.HeadID:
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
			}
		case StateDrafting:
			if drafting {
				e.state = StateDrafting
			} else {
				e.state = StateDraftFailed
				e.err = "drafting was interrupted"
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
	return effectiveState(p, r, has, s.drafting[p.RoomID], s.sending[p.RoomID], s.now()), r, has
}

// putLocked saves a record, logging and surfacing a failure. The store keeps
// the record in memory even when the write fails. s.mu must be held.
func (s *Service) putLocked(r Record) {
	if err := s.store.Put(r); err != nil {
		s.noteStoreErrLocked(err)
		return
	}
	s.storeErr = ""
}

func (s *Service) noteStoreErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteStoreErrLocked(err)
}

func (s *Service) noteStoreErrLocked(err error) {
	slog.Warn("replydesk: saving state failed", "err", err)
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
// since, or outside the lookback) are never shown.
func (s *Service) Items(all bool) []ItemView {
	s.mu.Lock()
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

// Status is the status bar.
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

// Rescan starts a background scan unless one is already running. When it
// finishes, the pending set is replaced and the newest items are drafted.
func (s *Service) Rescan(ctx context.Context) {
	s.mu.Lock()
	if s.scanning {
		s.mu.Unlock()
		return
	}
	s.scanning = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		scanCtx, cancel := context.WithTimeout(ctx, scanTimeout)
		defer cancel()
		res := scanRooms(scanCtx, s.api, s.me, s.lookback, s.now())
		s.finishScan(res)
	}()
}

func (s *Service) finishScan(res scanResult) {
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
	if s.preDraft <= 0 || s.drafter.Mode() == ModeOff {
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
		s.startDraftLocked(p, r, has, "", "")
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
func (s *Service) Draft(_ context.Context, roomID, instruction string) *APIError {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[roomID]
	if !ok {
		return &APIError{Code: CodeNotPending, Detail: "this conversation is not waiting for a reply"}
	}
	e, r, has := s.effectiveLocked(p)
	switch e.state {
	case StateSent, StateDismissed, StateSending:
		return &APIError{Code: CodeNotPending, Detail: "this conversation is already " + string(e.state)}
	case StateSendUnknown:
		// Regenerating would replace the send_unknown warning with a fresh
		// draft and invite a duplicate send.
		return &APIError{Code: CodeNotPending, Detail: "the last send may have gone through; check Webex, then edit and send or dismiss"}
	case StateDrafting:
		return nil
	}
	if s.drafter.Mode() == ModeOff {
		return &APIError{Code: "drafter_off", Detail: "drafting is off: the claude CLI is not available"}
	}
	prev := ""
	if has && r.Draft != nil {
		prev = r.Draft.Text
	}
	s.startDraftLocked(p, r, has, instruction, prev)
	return nil
}

// startDraftLocked records the room as drafting and runs the drafter in the
// background. s.mu must be held.
func (s *Service) startDraftLocked(p *Pending, r Record, has bool, instruction, previous string) {
	s.drafting[p.RoomID] = true
	rec := Record{RoomID: p.RoomID, HeadID: p.HeadID, State: StateDrafting}
	if has {
		rec.Draft = r.Draft
	}
	s.putLocked(rec)

	snap := *p
	s.wg.Add(1)
	go s.runDraft(snap, instruction, previous)
}

// runDraft grounds, drafts, and records the outcome for one room.
func (s *Service) runDraft(p Pending, instruction, previous string) {
	defer s.wg.Done()
	s.draftSem <- struct{}{}
	defer func() { <-s.draftSem }()

	ctx, cancel := context.WithTimeout(context.Background(), s.draftTimeout)
	defer cancel()

	in, refs := s.buildDraftInput(ctx, &p, instruction, previous)
	out, err := s.drafter.Draft(ctx, in)

	var (
		draft  *Draft
		errMsg string
	)
	switch {
	case err != nil:
		errMsg = capBytes(err.Error(), maxErrorLen)
		slog.Warn("replydesk: draft failed", "room_id", p.RoomID, "err", errMsg)
	case out == nil:
		errMsg = "drafter returned no output"
	case out.NeedsReply && strings.TrimSpace(out.Draft) == "":
		errMsg = "drafter returned an empty draft"
	default:
		draft = &Draft{
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
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.drafting, p.RoomID)
	r, has := s.store.Get(p.RoomID)
	switch {
	case !has || (r.HeadID == p.HeadID && r.State == StateDrafting):
		// The usual case. If the room's head moved on during drafting the
		// record still names the head the draft was written for, so the UI
		// shows the draft as stale.
		rec := Record{RoomID: p.RoomID, HeadID: p.HeadID}
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
	case draft != nil && r.State != StateSent && r.State != StateSending && r.State != StateSendUnknown:
		// The user dismissed or snoozed the room meanwhile: keep that, but
		// attach the draft.
		r.Draft = draft
		s.putLocked(r)
	}
}

// citations maps the drafter's cited refs back to messages.
func citations(cited []string, refs map[string]webex.Message) []Citation {
	out := []Citation{}
	seen := make(map[string]bool, len(cited))
	for _, ref := range cited {
		m, ok := refs[ref]
		if !ok || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, Citation{
			MessageID: m.ID,
			Created:   m.Created,
			Text:      capBytes(refText(&m), MaxCitationLen),
		})
	}
	return out
}
