package replydesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mythingies/plugin-webex/internal/safetext"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// ---- fake API ----

type apiCall struct {
	Method string
	Args   string
}

type sentCall struct {
	RoomID, ToPersonID, ToPersonEmail, ParentID, Text, Markdown string
}

// fakeAPI emulates the Webex endpoints Reply Desk uses and records every
// call. History is kept newest first, per room.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []apiCall
	me       webex.Person
	rooms    []webex.Space
	roomsErr error
	history  map[string][]webex.Message
	listErr  map[string]error
	getErrs  map[string]error
	getOver  map[string]*webex.Message
	sendErr  error
	sendID   string
	sends    []sentCall

	// Hooks run outside the lock, before the call is answered.
	roomsHook func()
	listHook  func(webex.ListMessagesOptions)
	sendHook  func()
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		me:      webex.Person{ID: meID, DisplayName: "Me Myself"},
		history: map[string][]webex.Message{},
		listErr: map[string]error{},
		getOver: map[string]*webex.Message{},
		getErrs: map[string]error{},
		sendID:  "sent-msg-1",
	}
}

func (f *fakeAPI) record(method, args string) {
	f.calls = append(f.calls, apiCall{method, args})
}

func (f *fakeAPI) addRoom(id, title string, newestFirst ...webex.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms = append(f.rooms, webex.Space{ID: id, Title: title, Type: "direct", LastActivity: testNow.Format(time.RFC3339)})
	f.history[id] = newestFirst
}

func (f *fakeAPI) setHistory(id string, newestFirst ...webex.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[id] = newestFirst
}

func (f *fakeAPI) prepend(id string, m webex.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[id] = append([]webex.Message{m}, f.history[id]...)
}

func (f *fakeAPI) GetMe() (*webex.Person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetMe", "")
	p := f.me
	return &p, nil
}

func (f *fakeAPI) ListDirectRooms(activeSince time.Time, limit int) ([]webex.Space, error) {
	if f.roomsHook != nil {
		f.roomsHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListDirectRooms", fmt.Sprintf("since=%s limit=%d", activeSince.UTC().Format(time.RFC3339), limit))
	return slices.Clone(f.rooms), f.roomsErr
}

func (f *fakeAPI) ListMessages(opts webex.ListMessagesOptions) ([]webex.Message, error) {
	if f.listHook != nil {
		f.listHook(opts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListMessages", fmt.Sprintf("room=%s before=%s max=%d", opts.RoomID, opts.BeforeMessage, opts.Max))
	if err := f.listErr[opts.RoomID]; err != nil {
		return nil, err
	}
	h := f.history[opts.RoomID]
	start := 0
	if opts.BeforeMessage != "" {
		i := slices.IndexFunc(h, func(m webex.Message) bool { return m.ID == opts.BeforeMessage })
		if i < 0 {
			return nil, nil
		}
		start = i + 1
	}
	n := opts.Max
	if n <= 0 {
		n = 50
	}
	end := min(start+n, len(h))
	return slices.Clone(h[start:end]), nil
}

func (f *fakeAPI) GetMessage(id string) (*webex.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetMessage", id)
	if err := f.getErrs[id]; err != nil {
		return nil, err
	}
	if m, ok := f.getOver[id]; ok {
		return m, nil
	}
	for _, h := range f.history {
		for i := range h {
			if h[i].ID == id {
				m := h[i]
				return &m, nil
			}
		}
	}
	return nil, webex.ErrNotFound
}

func (f *fakeAPI) SendMessage(roomID, toPersonID, toPersonEmail, parentID, text, markdown string) (*webex.Message, error) {
	if f.sendHook != nil {
		f.sendHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SendMessage", roomID)
	f.sends = append(f.sends, sentCall{roomID, toPersonID, toPersonEmail, parentID, text, markdown})
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &webex.Message{ID: f.sendID, RoomID: roomID, Text: text, PersonID: meID}, nil
}

// callsOf returns the args of every call to method.
func (f *fakeAPI) callsOf(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Args)
		}
	}
	return out
}

func (f *fakeAPI) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeAPI) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// ---- fake drafter ----

type fakeDrafter struct {
	mu         sync.Mutex
	mode       string
	inputs     []DraftInput
	fn         func(DraftInput) (*DraftOutput, error)
	gate       chan struct{} // when set, Draft blocks until it is closed
	running    int
	maxRunning int
}

func newFakeDrafter() *fakeDrafter { return &fakeDrafter{mode: ModeBare} }

func (d *fakeDrafter) Mode() string { return d.mode }

func (d *fakeDrafter) Draft(ctx context.Context, in DraftInput) (*DraftOutput, error) {
	d.mu.Lock()
	d.inputs = append(d.inputs, in)
	d.running++
	d.maxRunning = max(d.maxRunning, d.running)
	gate, fn := d.gate, d.fn
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running--
		d.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fn != nil {
		return fn(in)
	}
	return &DraftOutput{
		NeedsReply: true,
		Summary:    "asks something",
		Asks:       []string{"something"},
		Draft:      "Reply to " + in.AskerName,
	}, nil
}

func (d *fakeDrafter) calls() []DraftInput {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.inputs)
}

func (d *fakeDrafter) runningNow() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// ---- helpers ----

func newTestService(t *testing.T, api *fakeAPI, d Drafter, mods ...func(*Config)) (*Service, *fixedClock) {
	t.Helper()
	clock := &fixedClock{t: testNow}
	cfg := Config{API: api, Drafter: d, Now: clock.now, PreDraft: -1}
	for _, m := range mods {
		m(&cfg)
	}
	s, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.wait)
	return s, clock
}

func rescan(s *Service) {
	s.Rescan(context.Background())
	s.wait()
}

func itemFor(t *testing.T, s *Service, roomID string) ItemView {
	t.Helper()
	for _, it := range s.Items(true) {
		if it.RoomID == roomID {
			return it
		}
	}
	t.Fatalf("no item for room %s", roomID)
	return ItemView{}
}

func pendingIDs(ps []Pending) []string {
	out := make([]string, len(ps))
	for i := range ps {
		out[i] = ps[i].RoomID
	}
	return out
}

func itemIDs(items []ItemView) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].RoomID
	}
	return out
}

// seq returns n messages newest first, ids prefix0.., spaced step apart
// starting at start ago. mineAt marks indexes sent by the user.
func seq(prefix string, n int, start, step time.Duration, mineAt ...int) []webex.Message {
	out := make([]webex.Message, n)
	for i := range n {
		id := fmt.Sprintf("%s%d", prefix, i)
		ago := start + time.Duration(i)*step
		if slices.Contains(mineAt, i) {
			out[i] = mine(id, ago)
		} else {
			out[i] = theirs(id, ago)
		}
	}
	return out
}

func sendReq(room, text string) SendRequest {
	return SendRequest{RoomID: room, Text: text, TextSHA256: sha256Hex(text)}
}

func assertNoSend(t *testing.T, api *fakeAPI) {
	t.Helper()
	if n := len(api.callsOf("SendMessage")); n != 0 {
		t.Fatalf("SendMessage called %d times, want 0", n)
	}
}

// ---- Scan ----

func TestScanAnsweredSkippedAndBurstDetected(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("answered", "Bob", mine("a2", time.Minute), theirs("a1", time.Hour))
	api.addRoom("burst", "Alice", seq("b", 20, 10*time.Minute, time.Minute, 2)...)

	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if ids := pendingIDs(got); !slices.Equal(ids, []string{"burst"}) {
		t.Fatalf("pending = %v, want [burst]", ids)
	}
	p := got[0]
	if gotB := ids(p.Burst); !slices.Equal(gotB, []string{"b1", "b0"}) {
		t.Fatalf("burst = %v, want [b1 b0]", gotB)
	}
	if p.HeadID != "b0" || p.RoomTitle != "Alice" {
		t.Fatalf("head/title = %q/%q", p.HeadID, p.RoomTitle)
	}
	if n := len(api.callsOf("ListMessages")); n != 2 {
		t.Fatalf("ListMessages calls = %d, want 2 (one per room)", n)
	}
	if c := api.callsOf("ListDirectRooms"); len(c) != 1 || c[0] != fmt.Sprintf("since=%s limit=200", testNow.Add(-DefaultLookback).Format(time.RFC3339)) {
		t.Fatalf("ListDirectRooms calls = %v", c)
	}
	assertNoSend(t, api)
}

func TestScanPaging(t *testing.T) {
	tests := []struct {
		name       string
		n          int
		mineAt     []int
		step       time.Duration
		wantCalls  int
		wantBurst  int
		wantFlags  []string
		wantBefore []string // BeforeMessage of each call
	}{
		{
			name: "reply on page 2 with enough context", n: 120, mineAt: []int{60}, step: time.Minute,
			wantCalls: 2, wantBurst: MaxBurst, wantFlags: []string{FlagLongBurst},
			wantBefore: []string{"", "m49"},
		},
		{
			name: "reply near end of page 1 needs more context", n: 120, mineAt: []int{45}, step: time.Minute,
			wantCalls: 2, wantBurst: MaxBurst, wantFlags: []string{FlagLongBurst},
			wantBefore: []string{"", "m49"},
		},
		{
			name: "reply with context on page 1", n: 120, mineAt: []int{5}, step: time.Minute,
			wantCalls: 1, wantBurst: 5, wantFlags: []string{},
			wantBefore: []string{""},
		},
		{
			name: "200 message cap", n: 400, step: time.Minute,
			wantCalls: 4, wantBurst: MaxBurst, wantFlags: []string{FlagLongBurst, FlagNoHistory},
			wantBefore: []string{"", "m49", "m99", "m149"},
		},
		{
			name: "90 day stop", n: 400, step: 2 * 24 * time.Hour,
			wantCalls: 1, wantBurst: MaxBurst, wantFlags: []string{FlagLongBurst, FlagNoHistory},
			wantBefore: []string{""},
		},
		{
			name: "history ends", n: 70, step: time.Minute,
			wantCalls: 3, wantBurst: MaxBurst, wantFlags: []string{FlagLongBurst, FlagNoHistory},
			wantBefore: []string{"", "m49", "m69"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.addRoom("r", "Alice", seq("m", tc.n, time.Minute, tc.step, tc.mineAt...)...)
			got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("pending = %d, want 1", len(got))
			}
			calls := api.callsOf("ListMessages")
			if len(calls) != tc.wantCalls {
				t.Fatalf("ListMessages calls = %v, want %d", calls, tc.wantCalls)
			}
			for i, c := range calls {
				want := fmt.Sprintf("room=r before=%s max=50", tc.wantBefore[i])
				if c != want {
					t.Fatalf("call %d = %q, want %q", i, c, want)
				}
			}
			p := got[0]
			if len(p.Burst) != tc.wantBurst {
				t.Fatalf("burst len = %d, want %d", len(p.Burst), tc.wantBurst)
			}
			if !slices.Equal(p.Flags, tc.wantFlags) {
				t.Fatalf("flags = %v, want %v", p.Flags, tc.wantFlags)
			}
			if len(tc.mineAt) > 0 && len(p.Context) != ContextMessages {
				t.Fatalf("context len = %d, want %d", len(p.Context), ContextMessages)
			}
		})
	}
}

func TestScanCapCountsAtMost200(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", seq("m", 400, time.Minute, time.Minute)...)
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("Scan = %v, %v", got, err)
	}
	if !strings.HasPrefix(got[0].Reason, "200 messages") {
		t.Fatalf("reason = %q, want it to count 200 messages", got[0].Reason)
	}
}

func TestScanSkipsOldHeadAndBots(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("old", "Old", theirs("o1", 8*24*time.Hour))
	bot := theirs("bot1", time.Minute)
	bot.PersonEmail = "helper@webex.bot"
	api.addRoom("bot", "Bot", bot)
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if err != nil || len(got) != 0 {
		t.Fatalf("Scan = %v, %v; want nothing", pendingIDs(got), err)
	}
}

func TestScanRateLimitAborts(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("rl", "Limited", theirs("x", time.Minute))
	for i := 1; i <= 9; i++ {
		api.addRoom(fmt.Sprintf("a%d", i), "Other", theirs(fmt.Sprintf("a%dm", i), time.Duration(i)*time.Minute))
	}
	api.listErr["rl"] = &webex.RateLimitError{Status: 429, RetryAfter: time.Minute}
	api.listHook = func(o webex.ListMessagesOptions) {
		if o.RoomID != "rl" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	var rl *webex.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want a RateLimitError", err)
	}
	calls := api.callsOf("ListMessages")
	if len(calls) > scanConcurrency {
		t.Fatalf("ListMessages calls after the rate limit: %d calls %v", len(calls), calls)
	}
	for _, id := range pendingIDs(got) {
		if !slices.Contains([]string{"a1", "a2", "a3"}, id) {
			t.Fatalf("pending room %s was scanned after the abort", id)
		}
	}
}

func TestScanPerRoomErrorSkipped(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("bad", "Bad", theirs("x", time.Minute))
	api.addRoom("good", "Good", theirs("g", time.Minute))
	api.addRoom("bad2", "Bad2", theirs("y", time.Minute))
	api.listErr["bad"] = errors.New("webex API error (status 500)")
	api.listErr["bad2"] = errors.New("webex API error (status 502)")
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if ids := pendingIDs(got); !slices.Equal(ids, []string{"good"}) {
		t.Fatalf("pending = %v, want [good]", ids)
	}
	if err == nil || !strings.Contains(err.Error(), "room bad:") || !strings.Contains(err.Error(), "room bad2:") {
		t.Fatalf("err = %v, want both room errors joined", err)
	}
	var rl *webex.RateLimitError
	if errors.As(err, &rl) {
		t.Fatal("per-room error reported as a rate limit")
	}
}

func TestScanSortOrder(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("three", "C", theirs("c", 3*time.Hour))
	api.addRoom("one", "A", theirs("a", time.Minute))
	api.addRoom("two", "B", theirs("b", 2*time.Hour))
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if ids := pendingIDs(got); !slices.Equal(ids, []string{"one", "two", "three"}) {
		t.Fatalf("order = %v", ids)
	}
}

func TestScanContextCancel(t *testing.T) {
	api := newFakeAPI()
	for i := range 6 {
		api.addRoom(fmt.Sprintf("r%d", i), "X", theirs(fmt.Sprintf("m%d", i), time.Minute))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := Scan(ctx, api, meID, DefaultLookback, testNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 0 || len(api.callsOf("ListMessages")) != 0 {
		t.Fatalf("cancelled scan still fetched: %v %v", pendingIDs(got), api.callsOf("ListMessages"))
	}

	// Cancelled mid-scan: running rooms stop before their next page.
	api2 := newFakeAPI()
	api2.addRoom("r", "X", seq("m", 300, time.Minute, time.Minute)...)
	ctx2, cancel2 := context.WithCancel(context.Background())
	api2.listHook = func(webex.ListMessagesOptions) { cancel2() }
	_, err = Scan(ctx2, api2, meID, DefaultLookback, testNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(api2.callsOf("ListMessages")); n != 1 {
		t.Fatalf("ListMessages calls = %d, want 1", n)
	}
}

func TestScanListRoomsError(t *testing.T) {
	api := newFakeAPI()
	api.roomsErr = errors.New("boom")
	got, err := Scan(context.Background(), api, meID, DefaultLookback, testNow)
	if err == nil || len(got) != 0 {
		t.Fatalf("Scan = %v, %v", got, err)
	}
}

// ---- effective state ----

func TestEffectiveStateRules(t *testing.T) {
	p := &Pending{RoomID: "r", HeadID: "h2"}
	old := &Draft{HeadID: "h1", Text: "old"}
	cur := &Draft{HeadID: "h2", Text: "cur"}
	future := testNow.Add(time.Hour)
	past := testNow.Add(-time.Minute)
	tests := []struct {
		name               string
		r                  Record
		has                bool
		drafting, sending  bool
		want               State
		wantErr            string
		wantStale, wantDft bool
	}{
		{name: "no record", want: StatePending},
		{name: "no record, drafting in flight", drafting: true, want: StateDrafting},
		{name: "new head reopens dismissed", has: true, r: Record{HeadID: "h1", State: StateDismissed}, want: StatePending},
		{name: "new head reopens sent", has: true, r: Record{HeadID: "h1", State: StateSent, Error: "x"}, want: StatePending},
		{name: "new head reopens snoozed", has: true, r: Record{HeadID: "h1", State: StateSnoozed, SnoozeUntil: future}, want: StatePending},
		{name: "new head keeps stale draft", has: true, r: Record{HeadID: "h1", State: StateDrafted, Draft: old}, want: StatePending, wantStale: true, wantDft: true},
		{name: "same head drafted", has: true, r: Record{HeadID: "h2", State: StateDrafted, Draft: cur}, want: StateDrafted, wantDft: true},
		{name: "same head failed with old draft is stale", has: true, r: Record{HeadID: "h2", State: StateDraftFailed, Draft: old, Error: "e"}, want: StateDraftFailed, wantErr: "e", wantStale: true, wantDft: true},
		{name: "snoozed", has: true, r: Record{HeadID: "h2", State: StateSnoozed, SnoozeUntil: future}, want: StateSnoozed},
		{name: "snooze expired", has: true, r: Record{HeadID: "h2", State: StateSnoozed, SnoozeUntil: past}, want: StatePending},
		{name: "snooze expires exactly now", has: true, r: Record{HeadID: "h2", State: StateSnoozed, SnoozeUntil: testNow}, want: StatePending},
		{name: "leftover drafting", has: true, r: Record{HeadID: "h2", State: StateDrafting}, want: StateDraftFailed, wantErr: "drafting was interrupted"},
		{name: "drafting in flight", has: true, r: Record{HeadID: "h2", State: StateDrafting}, drafting: true, want: StateDrafting},
		{name: "leftover sending", has: true, r: Record{HeadID: "h2", State: StateSending}, want: StateSendUnknown, wantErr: "the send was interrupted; check Webex before sending again"},
		{name: "sending in flight", has: true, r: Record{HeadID: "h2", State: StateSending}, sending: true, want: StateSending},
		{name: "dismissed while drafting", has: true, r: Record{HeadID: "h2", State: StateDismissed}, drafting: true, want: StateDismissed},
		{name: "sent", has: true, r: Record{HeadID: "h2", State: StateSent}, want: StateSent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.RoomID = "r"
			e := effectiveState(p, tc.r, tc.has, tc.drafting, tc.sending, testNow)
			if e.state != tc.want || e.err != tc.wantErr || e.stale != tc.wantStale || (e.draft != nil) != tc.wantDft {
				t.Fatalf("got state=%s err=%q stale=%v draft=%v; want %s %q %v %v",
					e.state, e.err, e.stale, e.draft != nil, tc.want, tc.wantErr, tc.wantStale, tc.wantDft)
			}
		})
	}
}

func TestNewHeadReopensRoom(t *testing.T) {
	for _, action := range []string{"dismiss", "snooze", "send"} {
		t.Run(action, func(t *testing.T) {
			api := newFakeAPI()
			api.addRoom("r", "Alice", theirs("t1", time.Hour), mine("m0", 2*time.Hour))
			s, clock := newTestService(t, api, newFakeDrafter())
			rescan(s)
			switch action {
			case "dismiss":
				if e := s.Dismiss("r", DismissOther); e != nil {
					t.Fatal(e)
				}
			case "snooze":
				if e := s.Snooze("r", 24); e != nil {
					t.Fatal(e)
				}
			case "send":
				if _, _, e := s.Send(context.Background(), sendReq("r", "ok")); e != nil {
					t.Fatal(e)
				}
			}
			if st := itemFor(t, s, "r").State; st == StatePending {
				t.Fatalf("state after %s = pending", action)
			}
			// They write again (for send: after my reply).
			clock.set(testNow.Add(time.Minute))
			if action == "send" {
				api.setHistory("r", theirs("t2", -30*time.Second), mine("sent", 0), theirs("t1", time.Hour))
			} else {
				api.prepend("r", theirs("t2", -30*time.Second))
			}
			rescan(s)
			it := itemFor(t, s, "r")
			if it.State != StatePending {
				t.Fatalf("state after new message = %s, want pending", it.State)
			}
		})
	}
}

func TestSnoozeExpiry(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Hour))
	s, clock := newTestService(t, api, newFakeDrafter())
	rescan(s)
	if e := s.Snooze("r", 2); e != nil {
		t.Fatal(e)
	}
	it := itemFor(t, s, "r")
	if it.State != StateSnoozed || it.SnoozeUntil != testNow.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("item = %s until %q", it.State, it.SnoozeUntil)
	}
	clock.set(testNow.Add(2*time.Hour + time.Second))
	if it := itemFor(t, s, "r"); it.State != StatePending || it.SnoozeUntil != "" {
		t.Fatalf("after expiry: %s until %q", it.State, it.SnoozeUntil)
	}
	for _, h := range []int{0, 169} {
		if e := s.Snooze("r", h); e == nil || e.Code != codeBadRequest {
			t.Fatalf("Snooze(%d) = %v", h, e)
		}
	}
	if e := s.Snooze("nope", 1); e == nil || e.Code != CodeNotPending {
		t.Fatalf("Snooze(unknown) = %v", e)
	}
}

func TestDismissValidation(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Hour))
	s, _ := newTestService(t, api, newFakeDrafter())
	rescan(s)
	if e := s.Dismiss("r", "because"); e == nil || e.Code != codeBadRequest {
		t.Fatalf("bad reason = %v", e)
	}
	if e := s.Dismiss("nope", DismissOther); e == nil || e.Code != CodeNotPending {
		t.Fatalf("unknown room = %v", e)
	}
	if e := s.Dismiss("r", DismissReacted); e != nil {
		t.Fatal(e)
	}
	r, _ := s.store.Get("r")
	if r.State != StateDismissed || r.DismissReason != DismissReacted || r.HeadID != "t1" {
		t.Fatalf("record = %+v", r)
	}
}

func TestRestartLeftoverStates(t *testing.T) {
	path := storePath(t)
	st, err := OpenStore(path, meID)
	if err != nil {
		t.Fatal(err)
	}
	st.now = func() time.Time { return testNow }
	for _, r := range []Record{
		{RoomID: "drafting", HeadID: "d1", State: StateDrafting, Draft: &Draft{HeadID: "d1", Text: "kept"}},
		{RoomID: "sending", HeadID: "s1", State: StateSending},
		{RoomID: "oldhead", HeadID: "o0", State: StateSending},
	} {
		if err := st.Put(r); err != nil {
			t.Fatal(err)
		}
	}

	api := newFakeAPI()
	api.addRoom("drafting", "D", theirs("d1", time.Hour))
	api.addRoom("sending", "S", theirs("s1", time.Hour))
	api.addRoom("oldhead", "O", theirs("o1", time.Hour))
	s, _ := newTestService(t, api, newFakeDrafter(), func(c *Config) { c.StorePath = path })
	rescan(s)

	d := itemFor(t, s, "drafting")
	if d.State != StateDraftFailed || d.Error != "drafting was interrupted" || d.Draft == nil || d.Draft.Text != "kept" {
		t.Fatalf("drafting item = %+v", d)
	}
	if sn := itemFor(t, s, "sending"); sn.State != StateSendUnknown || sn.Error == "" {
		t.Fatalf("sending item = %+v", sn)
	}
	// send_unknown is not regenerated over...
	if e := s.Draft(context.Background(), "sending", ""); e == nil || e.Code != CodeNotPending {
		t.Fatalf("Draft in send_unknown = %v", e)
	}
	if o := itemFor(t, s, "oldhead"); o.State != StatePending {
		t.Fatalf("record for an older head = %s, want pending", o.State)
	}
	// ...but it can still be sent by hand.
	if _, status, e := s.Send(context.Background(), sendReq("sending", "hello")); e != nil {
		t.Fatalf("send from send_unknown = %d %v", status, e)
	}
}

func TestNewServicePrunesOldRecords(t *testing.T) {
	path := storePath(t)
	st, err := OpenStore(path, meID)
	if err != nil {
		t.Fatal(err)
	}
	st.now = func() time.Time { return testNow.Add(-31 * 24 * time.Hour) }
	_ = st.Put(Record{RoomID: "old", HeadID: "h", State: StateDismissed})
	st.now = func() time.Time { return testNow.Add(-29 * 24 * time.Hour) }
	_ = st.Put(Record{RoomID: "recent", HeadID: "h", State: StateDismissed})

	s, _ := newTestService(t, newFakeAPI(), nil, func(c *Config) { c.StorePath = path })
	if _, ok := s.store.Get("old"); ok {
		t.Fatal("31-day-old record survived")
	}
	if _, ok := s.store.Get("recent"); !ok {
		t.Fatal("29-day-old record was pruned")
	}
	if s.Status().DrafterMode != ModeOff {
		t.Fatal("nil drafter is not off")
	}
}

// ---- Draft ----

func TestDraftGroundingAndRank(t *testing.T) {
	// 400 messages an hour apart: burst m0,m1; my last reply m2.
	h := seq("m", 400, time.Minute, time.Hour, 2, 4, 50, 150, 250, 350)
	h[0].Text = "where is the staging deploy key stored?"
	h[1].Text = "quick question"
	h[50].Text = "The staging deploy key lives in vault under ops/staging"
	h[150].Text = "lunch on Friday sounds good to me"
	h[350].Text = "the deploy key rotation happens monthly via vault"
	api := newFakeAPI()
	api.addRoom("r", "Alice", h...)
	d := newFakeDrafter()
	s, _ := newTestService(t, api, d)
	rescan(s)
	api.resetCalls()

	if e := s.Draft(context.Background(), "r", ""); e != nil {
		t.Fatal(e)
	}
	s.wait()

	calls := api.callsOf("ListMessages")
	want := []string{"room=r before=m9 max=100", "room=r before=m109 max=100", "room=r before=m209 max=100"}
	if !slices.Equal(calls, want) {
		t.Fatalf("grounding calls = %v, want %v", calls, want)
	}
	in := d.calls()[0]
	if in.AskerName != "Alice" {
		t.Fatalf("asker = %q", in.AskerName)
	}
	if got := refsOf(in.Burst); !slices.Equal(got, []string{"b1", "b2"}) || in.Burst[0].Text != "quick question" {
		t.Fatalf("burst = %+v", in.Burst)
	}
	if len(in.Context) != ContextMessages || in.Context[0].Ref != "c1" || in.Context[len(in.Context)-1].Text != h[2].Text || !in.Context[len(in.Context)-1].FromMe {
		t.Fatalf("context = %+v", in.Context)
	}
	if len(in.Prior) == 0 || in.Prior[0].Ref != "p1" || in.Prior[0].Text != h[50].Text || !in.Prior[0].FromMe {
		t.Fatalf("prior = %+v", in.Prior)
	}
	for _, p := range in.Prior {
		if p.Text == h[350].Text {
			t.Fatal("prior includes a message past the 3-page grounding limit")
		}
	}
	if in.PreviousDraft != "" || in.Instruction != "" {
		t.Fatalf("first draft got previous=%q instruction=%q", in.PreviousDraft, in.Instruction)
	}
	it := itemFor(t, s, "r")
	if it.State != StateDrafted || it.Draft == nil || it.Draft.Text != "Reply to Alice" || it.Draft.Mode != ModeBare {
		t.Fatalf("item = %+v", it)
	}
	assertNoSend(t, api)
}

func refsOf(ms []RefMsg) []string {
	out := make([]string, len(ms))
	for i := range ms {
		out[i] = ms[i].Ref
	}
	return out
}

func TestDraftGroundingStopsAt180Days(t *testing.T) {
	h := seq("m", 400, time.Minute, 24*time.Hour, 2, 100, 200)
	h[0].Text = "what is the vault path for the deploy key?"
	h[1].Text = "and another thing"
	h[100].Text = "deploy key vault path is ops/one"
	h[200].Text = "deploy key vault path is ops/two"
	api := newFakeAPI()
	api.addRoom("r", "Alice", h...)
	d := newFakeDrafter()
	s, _ := newTestService(t, api, d, func(c *Config) { c.Lookback = 90 * 24 * time.Hour })
	rescan(s)
	api.resetCalls()
	if e := s.Draft(context.Background(), "r", ""); e != nil {
		t.Fatal(e)
	}
	s.wait()
	if calls := api.callsOf("ListMessages"); len(calls) != 2 {
		t.Fatalf("grounding calls = %v, want 2", calls)
	}
	in := d.calls()[0]
	if len(in.Prior) != 1 || in.Prior[0].Text != h[100].Text {
		t.Fatalf("prior = %+v, want only the message younger than 180 days", in.Prior)
	}
}

func TestDraftRefsAndCitations(t *testing.T) {
	api := newFakeAPI()
	long := strings.Repeat("é", 400) // 800 bytes
	h := []webex.Message{theirs("t2", time.Minute), theirs("t1", 2*time.Minute), mine("m1", time.Hour), theirs("t0", 2*time.Hour)}
	h[2].Text = long
	h[1].Text = ""
	h[1].Files = []string{"f1", "f2"}
	api.addRoom("r", "Alice", h...)
	d := newFakeDrafter()
	d.fn = func(in DraftInput) (*DraftOutput, error) {
		return &DraftOutput{NeedsReply: true, Draft: "ok", CitedRefs: []string{"c2", "zz", "c2", "b1"}}, nil
	}
	s, _ := newTestService(t, api, d)
	rescan(s)
	if e := s.Draft(context.Background(), "r", ""); e != nil {
		t.Fatal(e)
	}
	s.wait()
	in := d.calls()[0]
	if in.Burst[0].Text != "[shared 2 file(s)]" {
		t.Fatalf("placeholder = %q", in.Burst[0].Text)
	}
	r, _ := s.store.Get("r")
	if r.Draft == nil || len(r.Draft.Cited) != 2 {
		t.Fatalf("cited = %+v", r.Draft)
	}
	c := r.Draft.Cited[0]
	if c.MessageID != "m1" || c.Created != h[2].Created || len(c.Text) > MaxCitationLen || !utf8.ValidString(c.Text) || !strings.HasPrefix(long, c.Text) {
		t.Fatalf("citation = %+v (len %d)", c, len(c.Text))
	}
	if r.Draft.Cited[1].MessageID != "t1" {
		t.Fatalf("second citation = %+v", r.Draft.Cited[1])
	}
	it := itemFor(t, s, "r")
	if len(it.Draft.Cited) != 2 || it.Draft.Cited[0].Text != c.Text {
		t.Fatalf("view citations = %+v", it.Draft.Cited)
	}
}

func TestDraftFailures(t *testing.T) {
	longErr := strings.Repeat("x", 400)
	tests := []struct {
		name    string
		fn      func(DraftInput) (*DraftOutput, error)
		want    State
		wantErr string
	}{
		{"empty draft", func(DraftInput) (*DraftOutput, error) {
			return &DraftOutput{NeedsReply: true, Draft: "  \n"}, nil
		}, StateDraftFailed, "drafter returned an empty draft"},
		{"no reply needed may be empty", func(DraftInput) (*DraftOutput, error) {
			return &DraftOutput{NeedsReply: false, NeedsReplyReason: "just thanks"}, nil
		}, StateDrafted, ""},
		{"drafter error", func(DraftInput) (*DraftOutput, error) {
			return nil, errors.New(longErr)
		}, StateDraftFailed, longErr[:maxErrorLen]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.addRoom("r", "Alice", theirs("t1", time.Minute))
			d := newFakeDrafter()
			d.fn = tc.fn
			s, _ := newTestService(t, api, d)
			rescan(s)
			if e := s.Draft(context.Background(), "r", ""); e != nil {
				t.Fatal(e)
			}
			s.wait()
			it := itemFor(t, s, "r")
			if it.State != tc.want || it.Error != tc.wantErr {
				t.Fatalf("item = %s %q, want %s %q", it.State, it.Error, tc.want, tc.wantErr)
			}
		})
	}
}

func TestDraftOffAndNotPending(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	api.addRoom("d", "Dee", theirs("d1", time.Minute))
	s, _ := newTestService(t, api, offDrafter{}, func(c *Config) { c.PreDraft = 5 })
	rescan(s)
	if st := s.Status(); st.DrafterMode != ModeOff || st.Drafting != 0 {
		t.Fatalf("status = %+v", st)
	}
	if it := itemFor(t, s, "r"); it.State != StatePending {
		t.Fatalf("off drafter pre-drafted: %s", it.State)
	}
	if e := s.Draft(context.Background(), "r", ""); e == nil || e.Code != "drafter_off" {
		t.Fatalf("Draft with drafter off = %v", e)
	}
	if e := s.Draft(context.Background(), "nope", ""); e == nil || e.Code != CodeNotPending {
		t.Fatalf("Draft unknown = %v", e)
	}
	_ = s.Dismiss("d", DismissOther)
	if e := s.Draft(context.Background(), "d", ""); e == nil || e.Code != CodeNotPending {
		t.Fatalf("Draft dismissed = %v", e)
	}
}

func TestDraftRegenerate(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	n := 0
	d.fn = func(in DraftInput) (*DraftOutput, error) {
		n++
		return &DraftOutput{NeedsReply: true, Draft: fmt.Sprintf("v%d", n)}, nil
	}
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	s.wait()
	if e := s.Draft(context.Background(), "r", "shorter please"); e != nil {
		t.Fatal(e)
	}
	s.wait()
	in := d.calls()[1]
	if in.PreviousDraft != "v1" || in.Instruction != "shorter please" {
		t.Fatalf("regenerate input previous=%q instruction=%q", in.PreviousDraft, in.Instruction)
	}
	it := itemFor(t, s, "r")
	if it.Draft.Text != "v2" || it.Draft.Instruction != "shorter please" {
		t.Fatalf("draft = %+v", it.Draft)
	}
}

func TestDraftKeepsOldDraftWhileRegenerating(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	s.wait()
	d.mu.Lock()
	d.gate = make(chan struct{})
	d.mu.Unlock()
	_ = s.Draft(context.Background(), "r", "again")
	waitFor(t, func() bool { return d.runningNow() == 1 })
	it := itemFor(t, s, "r")
	if it.State != StateDrafting || it.Draft == nil || it.Draft.Text != "Reply to Alice" {
		t.Fatalf("while regenerating: %+v", it)
	}
	// A second request while drafting is a no-op.
	if e := s.Draft(context.Background(), "r", "x"); e != nil {
		t.Fatal(e)
	}
	close(d.gate)
	s.wait()
	if n := len(d.calls()); n != 2 {
		t.Fatalf("drafter calls = %d, want 2", n)
	}
}

func TestDraftSemaphoreAndPreDraft(t *testing.T) {
	api := newFakeAPI()
	for i := range 7 {
		api.addRoom(fmt.Sprintf("r%d", i), fmt.Sprintf("P%d", i), theirs(fmt.Sprintf("m%d", i), time.Duration(i+1)*time.Minute))
	}
	// The newest room has nothing to draft from.
	api.history["r0"][0].Text = ""
	api.history["r0"][0].Files = []string{"f"}
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d, func(c *Config) { c.PreDraft = 5 })
	s.Rescan(context.Background())
	waitFor(t, func() bool { return d.runningNow() == maxDraftsInFlight })
	time.Sleep(20 * time.Millisecond)
	if st := s.Status(); st.Drafting != 5 {
		t.Fatalf("drafting = %d, want 5", st.Drafting)
	}
	// A manual draft queues behind the same limit.
	if e := s.Draft(context.Background(), "r6", ""); e != nil {
		t.Fatal(e)
	}
	time.Sleep(20 * time.Millisecond)
	if n := d.runningNow(); n != maxDraftsInFlight {
		t.Fatalf("running = %d", n)
	}
	close(d.gate)
	s.wait()
	if d.maxRunning != maxDraftsInFlight {
		t.Fatalf("max concurrent drafts = %d, want %d", d.maxRunning, maxDraftsInFlight)
	}
	var drafted []string
	for _, in := range d.calls() {
		drafted = append(drafted, in.AskerName)
	}
	slices.Sort(drafted)
	if want := []string{"P1", "P2", "P3", "P4", "P5", "P6"}; !slices.Equal(drafted, want) {
		t.Fatalf("drafted %v, want %v", drafted, want)
	}
	if it := itemFor(t, s, "r0"); it.State != StatePending {
		t.Fatalf("attachment-only room = %s, want pending", it.State)
	}

	// A rescan drafts nothing new: every candidate has a current draft.
	d.gate = nil
	rescan(s)
	if n := len(d.calls()); n != 6 {
		t.Fatalf("drafter calls after rescan = %d, want 6", n)
	}
}

func TestDraftHeadChangedWhileDrafting(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	waitFor(t, func() bool { return d.runningNow() == 1 })

	api.prepend("r", theirs("t2", 0))
	s.Rescan(context.Background())
	waitFor(t, func() bool { return !s.Status().Scanning })
	if it := itemFor(t, s, "r"); it.State != StateDrafting {
		t.Fatalf("while drafting old head: %s", it.State)
	}
	close(d.gate)
	s.wait()
	r, _ := s.store.Get("r")
	if r.HeadID != "t1" || r.Draft == nil || r.Draft.HeadID != "t1" {
		t.Fatalf("record = %+v", r)
	}
	it := itemFor(t, s, "r")
	if it.State != StatePending || !it.DraftStale || it.Draft == nil {
		t.Fatalf("item = %s stale=%v draft=%v", it.State, it.DraftStale, it.Draft != nil)
	}
}

func TestDismissWhileDraftingKeepsDismissal(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	waitFor(t, func() bool { return d.runningNow() == 1 })
	if e := s.Dismiss("r", DismissNoReplyNeeded); e != nil {
		t.Fatal(e)
	}
	close(d.gate)
	s.wait()
	r, _ := s.store.Get("r")
	if r.State != StateDismissed || r.Draft == nil {
		t.Fatalf("record = %+v", r)
	}
}

// ---- Send ----

// sendFixture is a room with a drafted reply ready to send.
func sendFixture(t *testing.T) (*Service, *fakeAPI, *fixedClock) {
	t.Helper()
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t2", time.Minute), theirs("t1", 2*time.Minute), mine("m0", time.Hour))
	api.addRoom("r2", "Bob", theirs("u1", 3*time.Minute))
	s, clock := newTestService(t, api, newFakeDrafter())
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	s.wait()
	api.resetCalls()
	return s, api, clock
}

func TestSendCheckOrder(t *testing.T) {
	long := strings.Repeat("a", safetext.MaxMessageLen+1)
	tests := []struct {
		name   string
		req    SendRequest
		status int
		code   string
	}{
		{"unknown room", sendReq("nope", "hi"), http.StatusConflict, CodeNotPending},
		{"empty beats hash", SendRequest{RoomID: "r", Text: "  ", TextSHA256: "00"}, http.StatusBadRequest, CodeEmpty},
		{"hash beats fill", SendRequest{RoomID: "r", Text: "[[FILL: x]]", TextSHA256: "00"}, http.StatusBadRequest, CodeHashMismatch},
		{"fill beats too long", sendReq("r", "[[FILL: x]]"+long), http.StatusBadRequest, CodeFillRemaining},
		{"too long beats sanitize", sendReq("r", "javascript:x "+long), http.StatusBadRequest, CodeTooLong},
		{"sanitized", sendReq("r", "click javascript:alert(1)"), http.StatusConflict, CodeSanitized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := sendFixture(t)
			id, status, e := s.Send(context.Background(), tc.req)
			if e == nil || e.Code != tc.code || status != tc.status || id != "" {
				t.Fatalf("Send = %q %d %+v, want %d %s", id, status, e, tc.status, tc.code)
			}
			if tc.code == CodeSanitized && e.SanitizedText != safetext.Sanitize(tc.req.Text) {
				t.Fatalf("sanitized text = %q", e.SanitizedText)
			}
			if len(api.callsOf("GetMessage")) != 0 || len(api.callsOf("ListMessages")) != 0 {
				t.Fatal("text checks must run before the live re-check")
			}
			assertNoSend(t, api)
		})
	}
}

func TestSendRefusesDoneStates(t *testing.T) {
	s, api, _ := sendFixture(t)
	_ = s.Dismiss("r2", DismissOther)
	if _, status, e := s.Send(context.Background(), sendReq("r2", "hi")); e == nil || e.Code != CodeNotPending || status != http.StatusConflict {
		t.Fatalf("send dismissed = %d %v", status, e)
	}
	if _, _, e := s.Send(context.Background(), sendReq("r", "hi")); e != nil {
		t.Fatal(e)
	}
	if _, status, e := s.Send(context.Background(), sendReq("r", "hi again")); e == nil || e.Code != CodeNotPending || status != http.StatusConflict {
		t.Fatalf("second send = %d %v", status, e)
	}
	if n := api.sendCount(); n != 1 {
		t.Fatalf("sends = %d", n)
	}
}

func TestSendRefusesWhileDrafting(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	waitFor(t, func() bool { return d.runningNow() == 1 })
	if _, status, e := s.Send(context.Background(), sendReq("r", "hi")); e == nil || e.Code != CodeNotPending || status != http.StatusConflict {
		t.Fatalf("send while drafting = %d %v", status, e)
	}
	close(d.gate)
}

func TestSendSanitizedAckFlow(t *testing.T) {
	s, api, _ := sendFixture(t)
	text := "see javascript:alert(1) <@all>"
	_, _, e := s.Send(context.Background(), sendReq("r", text))
	if e == nil || e.Code != CodeSanitized {
		t.Fatalf("first send = %v", e)
	}
	// The UI resends the sanitized text, hashed, with the ack.
	req := sendReq("r", e.SanitizedText)
	req.AckSanitized = true
	id, status, e2 := s.Send(context.Background(), req)
	if e2 != nil || status != http.StatusOK || id == "" {
		t.Fatalf("ack send = %q %d %v", id, status, e2)
	}
	if got := api.sends[0].Text; got != safetext.Sanitize(text) || strings.Contains(got, "javascript:") {
		t.Fatalf("sent %q", got)
	}
}

func TestSendRecheck(t *testing.T) {
	edited := theirs("t2", time.Minute)
	edited.Updated = testNow.Format(time.RFC3339)
	tests := []struct {
		name   string
		setup  func(*fakeAPI)
		status int
		code   string
	}{
		{"deleted", func(a *fakeAPI) { a.getErrs["t2"] = fmt.Errorf("wrapped: %w", webex.ErrNotFound) }, http.StatusConflict, CodeAskDeleted},
		{"edited", func(a *fakeAPI) { a.getOver["t2"] = &edited }, http.StatusConflict, CodeAskEdited},
		{"already replied", func(a *fakeAPI) { a.prepend("r", mine("m9", 0)) }, http.StatusConflict, CodeAlreadyReplied},
		{"replied and new message", func(a *fakeAPI) {
			a.prepend("r", mine("m9", 20*time.Second))
			a.prepend("r", theirs("t3", 10*time.Second))
		}, http.StatusConflict, CodeAlreadyReplied},
		{"new messages", func(a *fakeAPI) { a.prepend("r", theirs("t3", 0)) }, http.StatusConflict, CodeNewMessages},
		{"head beyond the page", func(a *fakeAPI) {
			for i := range 25 {
				a.prepend("r", theirs(fmt.Sprintf("n%d", i), -time.Duration(i+1)*time.Second))
			}
		}, http.StatusConflict, CodeNewMessages},
		{"get rate limited", func(a *fakeAPI) { a.getErrs["t2"] = &webex.RateLimitError{Status: 429, RetryAfter: time.Minute} }, http.StatusTooManyRequests, CodeRateLimited},
		{"list rate limited", func(a *fakeAPI) { a.listErr["r"] = &webex.RateLimitError{Status: 429} }, http.StatusTooManyRequests, CodeRateLimited},
		{"list failed", func(a *fakeAPI) { a.listErr["r"] = errors.New("webex API error (status 500)") }, http.StatusBadGateway, codeRecheckFailed},
		{"get failed", func(a *fakeAPI) { a.getErrs["t2"] = errors.New("executing request: timeout") }, http.StatusBadGateway, codeRecheckFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := sendFixture(t)
			tc.setup(api)
			_, status, e := s.Send(context.Background(), sendReq("r", "hi"))
			if e == nil || e.Code != tc.code || status != tc.status {
				t.Fatalf("Send = %d %+v, want %d %s", status, e, tc.status, tc.code)
			}
			assertNoSend(t, api)
			if st := itemFor(t, s, "r").State; st != StateDrafted {
				t.Fatalf("state after refused send = %s", st)
			}
			// The refused send did not use up the rate limit.
			if _, status, e := s.Send(context.Background(), sendReq("r2", "hi")); e != nil {
				t.Fatalf("next send = %d %v", status, e)
			}
		})
	}
}

func TestSendRecheckOrderOfCalls(t *testing.T) {
	s, api, _ := sendFixture(t)
	if _, _, e := s.Send(context.Background(), sendReq("r", "hi")); e != nil {
		t.Fatal(e)
	}
	api.mu.Lock()
	calls := slices.Clone(api.calls)
	api.mu.Unlock()
	want := []apiCall{{"GetMessage", "t2"}, {"ListMessages", "room=r before= max=20"}, {"SendMessage", "r"}}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestSendRateLimit(t *testing.T) {
	api := newFakeAPI()
	for i := range 4 {
		api.addRoom(fmt.Sprintf("r%d", i), "X", theirs(fmt.Sprintf("m%d", i), time.Minute))
	}
	s, clock := newTestService(t, api, newFakeDrafter())
	rescan(s)
	if _, _, e := s.Send(context.Background(), sendReq("r0", "a")); e != nil {
		t.Fatal(e)
	}
	clock.set(testNow.Add(time.Second))
	if _, status, e := s.Send(context.Background(), sendReq("r1", "b")); e == nil || e.Code != CodeRateLimited || status != http.StatusTooManyRequests {
		t.Fatalf("send within 2s = %d %v", status, e)
	}
	clock.set(testNow.Add(2 * time.Second))
	if _, _, e := s.Send(context.Background(), sendReq("r1", "b")); e != nil {
		t.Fatalf("send after 2s = %v", e)
	}

	// 30 per rolling hour.
	base := testNow.Add(10 * time.Second)
	s.mu.Lock()
	s.sendTimes = nil
	for i := range sendMaxInWindow {
		s.sendTimes = append(s.sendTimes, base.Add(time.Duration(i)*time.Second))
	}
	s.mu.Unlock()
	clock.set(base.Add(time.Minute))
	if _, _, e := s.Send(context.Background(), sendReq("r2", "c")); e == nil || e.Code != CodeRateLimited {
		t.Fatalf("31st send in an hour = %v", e)
	}
	clock.set(base.Add(time.Hour))
	if _, _, e := s.Send(context.Background(), sendReq("r2", "c")); e != nil {
		t.Fatalf("send once the oldest left the window = %v", e)
	}
	if n := api.sendCount(); n != 3 {
		t.Fatalf("sends = %d, want 3", n)
	}
}

func TestSendSuccess(t *testing.T) {
	logs := captureSlog(t)
	s, api, _ := sendFixture(t)
	text := "Thanks Alice, here is the plan"
	id, status, e := s.Send(context.Background(), sendReq("r", text))
	if e != nil || status != http.StatusOK || id != "sent-msg-1" {
		t.Fatalf("Send = %q %d %v", id, status, e)
	}
	sc := api.sends[0]
	if sc != (sentCall{RoomID: "r", Text: text}) {
		t.Fatalf("SendMessage args = %+v", sc)
	}
	r, _ := s.store.Get("r")
	if r.State != StateSent || r.SentMessageID != "sent-msg-1" || r.SentSHA256 != sha256Hex(text) || r.Draft == nil || r.Draft.Text != text || r.HeadID != "t2" {
		t.Fatalf("record = %+v", r)
	}
	if r.Draft.Summary != "asks something" {
		t.Fatalf("draft details were lost: %+v", r.Draft)
	}
	out := logs.String()
	for _, want := range []string{"msg=audit", "tool=replydesk", "action=send", "room_id=r", "message_id=sent-msg-1", fmt.Sprintf("bytes=%d", len(text)), "sha256=" + sha256Hex(text)} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, text) {
		t.Fatal("message text was logged")
	}
	if items := s.Items(false); slices.Contains(itemIDs(items), "r") {
		t.Fatal("sent room still listed without all")
	}
	if it := itemFor(t, s, "r"); it.State != StateSent {
		t.Fatalf("state = %s", it.State)
	}
}

func TestSendInThread(t *testing.T) {
	api := newFakeAPI()
	reply := theirs("t2", time.Minute)
	reply.ParentID = "root1"
	api.addRoom("r", "Alice", reply, theirs("root1", time.Hour))
	s, _ := newTestService(t, api, newFakeDrafter())
	rescan(s)
	if it := itemFor(t, s, "r"); !it.InThread || !it.Burst[1].InThread {
		t.Fatalf("item not in thread: %+v", it)
	}
	if _, _, e := s.Send(context.Background(), sendReq("r", "hi")); e != nil {
		t.Fatal(e)
	}
	if api.sends[0].ParentID != "root1" || api.sends[0].RoomID != "r" {
		t.Fatalf("send = %+v", api.sends[0])
	}
}

func TestSendErrorIsSendUnknown(t *testing.T) {
	s, api, clock := sendFixture(t)
	api.sendErr = errors.New("executing request: connection reset")
	_, status, e := s.Send(context.Background(), sendReq("r", "hi"))
	if e == nil || e.Code != CodeSendUnknown || status != http.StatusBadGateway {
		t.Fatalf("Send = %d %v", status, e)
	}
	if n := api.sendCount(); n != 1 {
		t.Fatalf("SendMessage calls = %d, want exactly 1 (no retry)", n)
	}
	it := itemFor(t, s, "r")
	if it.State != StateSendUnknown || it.Error == "" || it.Draft == nil {
		t.Fatalf("item = %+v", it)
	}
	// Nothing retries it in the background, even across a rescan.
	clock.set(testNow.Add(5 * time.Second))
	rescan(s)
	if n := api.sendCount(); n != 1 {
		t.Fatalf("SendMessage calls after rescan = %d", n)
	}
	// The user may send again by hand after being warned.
	api.sendErr = nil
	if _, _, e := s.Send(context.Background(), sendReq("r", "hi")); e != nil {
		t.Fatalf("manual resend = %v", e)
	}
}

func TestSendPerRoomLock(t *testing.T) {
	s, api, clock := sendFixture(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	api.sendHook = func() {
		entered <- struct{}{}
		<-release
	}
	type result struct {
		status int
		e      *APIError
	}
	first := make(chan result, 1)
	go func() {
		_, st, e := s.Send(context.Background(), sendReq("r", "one"))
		first <- result{st, e}
	}()
	<-entered
	if st := itemFor(t, s, "r").State; st != StateSending {
		t.Fatalf("state during send = %s", st)
	}
	clock.set(testNow.Add(10 * time.Second)) // not the rate limit
	second := make(chan result, 1)
	go func() {
		_, st, e := s.Send(context.Background(), sendReq("r", "two"))
		second <- result{st, e}
	}()
	select {
	case <-entered:
		t.Fatal("second send reached SendMessage while the first was in flight")
	case r := <-second:
		t.Fatalf("second send returned before the first finished: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if r := <-first; r.e != nil {
		t.Fatalf("first send = %+v", r)
	}
	if r := <-second; r.e == nil || r.e.Code != CodeNotPending || r.status != http.StatusConflict {
		t.Fatalf("second send = %+v", r)
	}
	if n := api.sendCount(); n != 1 {
		t.Fatalf("sends = %d", n)
	}
}

// ---- Items and Status ----

func TestItemsJSONHasNoNulls(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.fn = func(DraftInput) (*DraftOutput, error) { return &DraftOutput{NeedsReply: true, Draft: "x"}, nil }
	s, _ := newTestService(t, api, d)
	rescan(s)
	check := func() {
		t.Helper()
		b, err := json.Marshal(s.Items(true))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "null") {
			t.Fatalf("items JSON has null: %s", b)
		}
		for _, want := range []string{`"flags":["no_history"]`, `"context":[]`} {
			if !strings.Contains(string(b), want) {
				t.Fatalf("items JSON missing %s: %s", want, b)
			}
		}
	}
	check()
	_ = s.Draft(context.Background(), "r", "")
	s.wait()
	check()
	b, _ := json.Marshal(s.Items(true))
	for _, want := range []string{`"asks":[]`, `"missing_info":[]`, `"cited":[]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("draft JSON missing %s: %s", want, b)
		}
	}
	s2, _ := newTestService(t, newFakeAPI(), d)
	empty, _ := json.Marshal(s2.Items(false))
	if string(empty) != "[]" {
		t.Fatalf("empty items = %s", empty)
	}
}

func TestItemsSortAndHide(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("pending-old", "A", theirs("a", 5*time.Hour))
	api.addRoom("drafted-new", "B", theirs("b", time.Minute))
	api.addRoom("snoozed-new", "C", theirs("c", 2*time.Minute))
	api.addRoom("sent", "D", theirs("d", 3*time.Minute))
	api.addRoom("dismissed", "E", theirs("e", 4*time.Minute))
	api.addRoom("failed", "F", theirs("f", 6*time.Hour))
	d := newFakeDrafter()
	d.fn = func(in DraftInput) (*DraftOutput, error) {
		if in.AskerName == "F" {
			return nil, errors.New("nope")
		}
		return &DraftOutput{NeedsReply: true, Draft: "x"}, nil
	}
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "drafted-new", "")
	_ = s.Draft(context.Background(), "failed", "")
	s.wait()
	_ = s.Snooze("snoozed-new", 3)
	_ = s.Dismiss("dismissed", DismissOther)
	if _, _, e := s.Send(context.Background(), sendReq("sent", "hi")); e != nil {
		t.Fatal(e)
	}
	if got, want := itemIDs(s.Items(false)), []string{"drafted-new", "pending-old", "failed", "snoozed-new"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if got, want := itemIDs(s.Items(true)), []string{"drafted-new", "pending-old", "failed", "snoozed-new", "sent", "dismissed"}; !slices.Equal(got, want) {
		t.Fatalf("all items = %v, want %v", got, want)
	}
}

func TestItemMsgViews(t *testing.T) {
	api := newFakeAPI()
	edited := theirs("t3", time.Minute)
	edited.Updated = ts(30 * time.Second)
	card := theirs("t2", 2*time.Minute)
	card.Attachments = []json.RawMessage{json.RawMessage(`{}`)}
	voice := theirs("t1", 3*time.Minute)
	voice.IsVoiceClip = true
	voice.Files = []string{"a", "b"}
	api.addRoom("r", "Alice", edited, card, voice, mine("m0", time.Hour))
	s, _ := newTestService(t, api, newFakeDrafter())
	rescan(s)
	it := itemFor(t, s, "r")
	if it.Title != "Alice" || it.Since != testNow.Add(-3*time.Minute).Format(time.RFC3339) || it.HeadCreated != testNow.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("item = %+v", it)
	}
	b := it.Burst
	if len(b) != 3 || b[0].Files != 2 || !b[0].VoiceClip || !b[1].Card || !b[2].Edited || b[2].Author != "Alice" || b[2].FromMe {
		t.Fatalf("burst views = %+v", b)
	}
	c := it.Context
	if len(c) != 1 || c[0].Author != "You" || !c[0].FromMe || c[0].Text != "text m0" {
		t.Fatalf("context views = %+v", c)
	}
	if !slices.Equal(it.Flags, []string{FlagAttachment, FlagCard, FlagVoiceClip, FlagEdited}) {
		t.Fatalf("flags = %v", it.Flags)
	}
}

func TestStatus(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	gate := make(chan struct{})
	api.roomsHook = func() { <-gate }
	s, _ := newTestService(t, api, newFakeDrafter(), func(c *Config) { c.Lookback = 36 * time.Hour })
	st := s.Status()
	if st.Me != "Me Myself" || st.DrafterMode != ModeBare || st.Lookback != "36h0m0s" || st.MaxMessageLen != 7439 || st.LastScan != "" || st.Scanning {
		t.Fatalf("status = %+v", st)
	}
	s.Rescan(context.Background())
	s.Rescan(context.Background()) // single flight
	if !s.Status().Scanning {
		t.Fatal("not scanning")
	}
	close(gate)
	s.wait()
	if n := len(api.callsOf("ListDirectRooms")); n != 1 {
		t.Fatalf("scans = %d, want 1", n)
	}
	st = s.Status()
	if st.Scanning || st.LastScan != testNow.Format(time.RFC3339) || st.Error != "" {
		t.Fatalf("status after scan = %+v", st)
	}

	for d, want := range map[time.Duration]string{
		DefaultLookback:     "7d",
		24 * time.Hour:      "1d",
		90 * time.Minute:    "1h30m0s",
		36 * time.Hour:      "36h0m0s",
		90 * 24 * time.Hour: "90d",
	} {
		if got := fmtLookback(d); got != want {
			t.Fatalf("fmtLookback(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestRescanErrorsKeepPreviousResults(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("a", "A", theirs("a1", time.Minute))
	api.addRoom("b", "B", theirs("b1", 2*time.Minute))
	s, _ := newTestService(t, api, newFakeDrafter())
	rescan(s)

	api.addRoom("c", "C", theirs("c1", 3*time.Minute))
	rescan(s)
	api.listErr["a"] = errors.New("webex API error (status 500)")
	api.setHistory("b", mine("b2", 0), theirs("b1", 2*time.Minute)) // answered
	api.mu.Lock()
	api.rooms = slices.DeleteFunc(api.rooms, func(r webex.Space) bool { return r.ID == "c" }) // aged out
	api.mu.Unlock()
	rescan(s)
	if got := itemIDs(s.Items(true)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("items = %v, want only [a] kept from the last scan", got)
	}
	if st := s.Status(); !strings.Contains(st.Error, "1 conversation(s) could not be scanned") {
		t.Fatalf("status error = %q", st.Error)
	}

	api.listErr = map[string]error{}
	api.roomsErr = &webex.RateLimitError{Status: 429, RetryAfter: 30 * time.Second}
	rescan(s)
	if got := itemIDs(s.Items(true)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("items after a rate-limited scan = %v", got)
	}
	if st := s.Status(); !strings.Contains(st.Error, "rate limited") {
		t.Fatalf("status error = %q", st.Error)
	}

	api.roomsErr = nil
	api.setHistory("a", mine("a2", 0), theirs("a1", time.Minute))
	rescan(s)
	if got := s.Items(true); len(got) != 0 || s.Status().Error != "" {
		t.Fatalf("items = %v, error %q", itemIDs(got), s.Status().Error)
	}
}

func TestServiceOnlyGETsUntilSend(t *testing.T) {
	s, api, _ := sendFixture(t)
	_ = s.Items(true)
	_ = s.Status()
	rescan(s)
	_ = s.Snooze("r2", 1)
	_ = s.Dismiss("r2", DismissOther)
	assertNoSend(t, api)
}
