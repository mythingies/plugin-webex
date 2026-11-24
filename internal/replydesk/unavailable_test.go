package replydesk

// Tests for a process that must stop serving its inbox (signed out
// elsewhere, or the store taken over by another --review), and for drafts
// that finish while a send that does not go out holds the room.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

// setStoreRename replaces the store's rename (the last step of every disk
// write) without racing a write in flight.
func setStoreRename(st *Store, fn func(oldpath, newpath string) error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.rename = fn
}

// waitUntil polls cond for up to 10s. Unlike waitFor it does not fail the
// test, so it is safe off the test goroutine (e.g. in the store's writer).
func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// ---- R3: signed out elsewhere stops scanning and hides the DMs ----

func TestSignedOutHidesItemsAndStopsScanning(t *testing.T) {
	s, api, _ := sendFixture(t)
	if len(s.Items(true)) == 0 {
		t.Fatal("fixture lists no items")
	}
	if s.SignedOut() {
		t.Fatal("SignedOut before sign-out")
	}
	s.mu.Lock()
	s.signedOut = func() bool { return true }
	s.mu.Unlock()

	for _, all := range []bool{false, true} {
		if items := s.Items(all); items == nil || len(items) != 0 {
			t.Fatalf("Items(%v) after sign-out = %v, want an empty list", all, itemIDs(items))
		}
	}
	if !s.SignedOut() {
		t.Fatal("SignedOut = false after sign-out")
	}
	api.resetCalls()
	rescan(s)
	if n := len(api.callsOf("ListDirectRooms")) + len(api.callsOf("ListMessages")); n != 0 {
		t.Fatalf("a scan ran after sign-out: %d Webex calls", n)
	}
	if s.Status().Scanning {
		t.Fatal("status shows scanning after sign-out")
	}
}

// A scan that was running when the sign-out was noticed keeps none of its
// results.
func TestScanFinishingAfterSignOutKeepsNothing(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	s, _ := newTestService(t, api, newFakeDrafter())
	rescan(s)
	api.roomsHook = func() { s.noteAuthErr(fmt.Errorf("getting access token: %w", auth.ErrSignedOut)) }
	rescan(s)
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pending rooms kept after sign-out, want 0", n)
	}
}

// ---- R5/R7: another --review took over the store ----

func lockLostFixture(t *testing.T) (*Service, *fakeAPI, string) {
	t.Helper()
	path := storePath(t)
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	api.addRoom("r2", "Bob", theirs("u1", 3*time.Minute))
	s, _ := newTestService(t, api, newFakeDrafter(), func(c *Config) { c.StorePath = path })
	rescan(s)
	api.resetCalls()
	return s, api, path
}

// takeOver replaces the store's lock with another process's, as a second
// --review does after this one's heartbeat stalled (laptop sleep).
func takeOver(t *testing.T, path string) {
	t.Helper()
	lock := filepath.Join(filepath.Dir(path), lockFileName)
	if err := os.WriteFile(lock, []byte("pid=1\nnonce=other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLockLostSurfacesAndBlocks(t *testing.T) {
	s, api, path := lockLostFixture(t)
	takeOver(t, path)

	if st := s.Status(); st.Error != msgLockLost {
		t.Fatalf("status error = %q, want %q", st.Error, msgLockLost)
	}
	if s.SignedOut() {
		t.Fatal("a takeover reported as a sign-out")
	}
	if _, status, e := s.Send(context.Background(), sendReq(s, "r", "hi")); e == nil || e.Code != CodeLockLost || status != http.StatusConflict {
		t.Fatalf("Send = %d %+v, want 409 lock_lost", status, e)
	}
	if e := s.Draft(context.Background(), "r2", ""); e == nil || e.Code != CodeLockLost {
		t.Fatalf("Draft = %+v", e)
	}
	if e := s.Dismiss("r2", DismissOther); e == nil || e.Code != CodeLockLost {
		t.Fatalf("Dismiss = %+v", e)
	}
	if e := s.Snooze("r2", 1); e == nil || e.Code != CodeLockLost {
		t.Fatalf("Snooze = %+v", e)
	}
	// A scan still runs (its results stay in memory), but pre-drafts nothing.
	s.mu.Lock()
	s.preDraft = 5
	s.mu.Unlock()
	rescan(s)
	if n := len(api.callsOf("ListDirectRooms")); n != 1 {
		t.Fatalf("%d room listings after the takeover, want 1", n)
	}
	for _, room := range []string{"r", "r2"} {
		if st := itemFor(t, s, room).State; st != StatePending {
			t.Fatalf("room %s is %s after the takeover, want pending (no pre-draft)", room, st)
		}
	}
	assertNoSend(t, api)
	// The state is kept in memory: this is not a purge.
	if got := itemIDs(s.Items(true)); len(got) != 2 {
		t.Fatalf("items after takeover = %v, want both rooms", got)
	}
	if exists(path) {
		t.Fatal("the store file was written after the takeover")
	}
}

// The takeover lands between the pre-send checks and the durable sending
// record: the save is refused, nothing is posted, and the answer says why.
func TestSendRefusedWhenLockLostAtSave(t *testing.T) {
	s, api, path := lockLostFixture(t)
	s.mu.Lock()
	s.lockLost = func() bool { return false } // the service has not noticed yet
	s.mu.Unlock()
	takeOver(t, path)

	_, status, e := s.Send(context.Background(), sendReq(s, "r", "hi"))
	if e == nil || e.Code != CodeLockLost || status != http.StatusConflict {
		t.Fatalf("Send = %d %+v, want 409 lock_lost", status, e)
	}
	assertNoSend(t, api)
	if st := itemFor(t, s, "r").State; st != StatePending {
		t.Fatalf("state after the refused send = %s, want pending", st)
	}
	if !s.store.LockLost() {
		t.Fatal("store does not report the lost lock")
	}
}

// ---- R8: a draft finishing while a send that does not go out holds the room ----

// The draft finishes inside the send's durable write, which then fails:
// nothing is sent, and the room shows the finished draft, not "drafting
// was interrupted".
func TestDraftFinishingDuringFailedSendIsKept(t *testing.T) {
	path := storePath(t)
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d, func(c *Config) { c.StorePath = path })
	rescan(s)
	if e := s.Draft(context.Background(), "r", ""); e != nil {
		t.Fatal(e)
	}
	waitFor(t, func() bool { return d.runningNow() == 1 })

	var (
		once     sync.Once
		finished bool
	)
	setStoreRename(s.store, func(oldpath, newpath string) error {
		if r, _ := s.store.Get("r"); r.State != StateSending {
			return os.Rename(oldpath, newpath)
		}
		// Every write of the sending record fails (whichever goroutine
		// makes it); the first one lets the draft finish meanwhile.
		once.Do(func() {
			close(d.gate)
			finished = waitUntil(func() bool { return s.Status().Drafting == 0 })
		})
		return errors.New("disk full")
	})
	t.Cleanup(func() { setStoreRename(s.store, os.Rename) })

	_, status, e := s.Send(context.Background(), sendReq(s, "r", "my words"))
	if e == nil || e.Code != codeInternal || status != http.StatusInternalServerError {
		t.Fatalf("Send = %d %+v, want 500 internal", status, e)
	}
	assertNoSend(t, api)
	s.wait()
	setStoreRename(s.store, os.Rename) // also orders the read of finished
	if !finished {
		t.Fatal("the draft did not finish during the send")
	}
	it := itemFor(t, s, "r")
	if it.State != StateDrafted || it.Draft == nil || it.Draft.Text != "Reply to Alice" || it.Error != "" {
		t.Fatalf("item after the failed send = state %s, error %q, draft %+v; want the finished draft", it.State, it.Error, it.Draft)
	}
	s.mu.Lock()
	held := len(s.heldDrafts)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d held drafts left", held)
	}
}

// The same when the token provider refuses the send (signed out): the room
// is put back with the draft that finished meanwhile.
func TestDraftFinishingDuringSignedOutSendIsKept(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d)
	rescan(s)
	if e := s.Draft(context.Background(), "r", ""); e != nil {
		t.Fatal(e)
	}
	waitFor(t, func() bool { return d.runningNow() == 1 })
	api.sendErr = fmt.Errorf("getting access token: %w", auth.ErrSignedOut)
	api.sendHook = func() {
		close(d.gate)
		waitFor(t, func() bool { return s.Status().Drafting == 0 })
	}

	if _, status, e := s.Send(context.Background(), sendReq(s, "r", "my words")); e == nil || e.Code != CodeSignedOut || status != http.StatusConflict {
		t.Fatalf("Send = %d %+v, want 409 signed_out", status, e)
	}
	s.wait()
	r, ok := s.store.Get("r")
	if !ok || r.State != StateDrafted || r.Draft == nil || r.Draft.Text != "Reply to Alice" {
		t.Fatalf("record after the refused send = %+v, want the finished draft", r)
	}
	s.mu.Lock()
	held := len(s.heldDrafts)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d held drafts left", held)
	}
}

// A send that goes out discards the held draft.
func TestDraftFinishingDuringSuccessfulSendIsDropped(t *testing.T) {
	api := newFakeAPI()
	api.addRoom("r", "Alice", theirs("t1", time.Minute))
	d := newFakeDrafter()
	d.gate = make(chan struct{})
	s, _ := newTestService(t, api, d)
	rescan(s)
	_ = s.Draft(context.Background(), "r", "")
	waitFor(t, func() bool { return d.runningNow() == 1 })
	api.sendHook = func() {
		close(d.gate)
		waitFor(t, func() bool { return s.Status().Drafting == 0 })
	}
	if _, _, e := s.Send(context.Background(), sendReq(s, "r", "mine")); e != nil {
		t.Fatal(e)
	}
	s.wait()
	if r, _ := s.store.Get("r"); r.State != StateSent || r.Draft == nil || r.Draft.Text != "mine" {
		t.Fatalf("record = %+v", r)
	}
	s.mu.Lock()
	held := len(s.heldDrafts)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d held drafts left after a sent reply", held)
	}
}
