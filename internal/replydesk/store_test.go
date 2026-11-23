package replydesk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const testOwner = "person-owner"

// fixedClock returns a clock that reports t and can be moved.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sub", "replydesk.json")
}

// mustOpen opens a store and closes it when the test ends. Close is
// idempotent, so tests may also close it themselves.
func mustOpen(t *testing.T, path, owner string) *Store {
	t.Helper()
	s, err := OpenStore(path, owner)
	if err != nil {
		t.Fatalf("OpenStore(%q, %q): %v", path, owner, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustClose closes s, failing the test on error.
func mustClose(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// reopen closes s (flushing it and releasing the lock) and opens its path
// again for owner.
func reopen(t *testing.T, s *Store, owner string) *Store {
	t.Helper()
	mustClose(t, s)
	return mustOpen(t, s.path, owner)
}

// tmpFiles lists the store's temp files (both the old fixed name and the
// CreateTemp pattern).
func tmpFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(path)+".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

// setVar sets a package variable for the duration of the test.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// eventually polls cond until it holds or 10s pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fileHasRoom reports whether the store file on disk holds room.
func fileHasRoom(path, room string) bool {
	data, err := os.ReadFile(path) // #nosec G304 -- test temp path
	if err != nil {
		return false
	}
	var f storeFile
	if json.Unmarshal(data, &f) != nil {
		return false
	}
	_, ok := f.Records[room]
	return ok
}

func sampleRecord(room string) Record {
	return Record{
		RoomID: room,
		HeadID: "head-" + room,
		State:  StateDrafted,
		Draft: &Draft{
			HeadID:      "head-" + room,
			Text:        "Sure, [[FILL: time]] works.",
			Summary:     "asks for a meeting",
			Asks:        []string{"meet?"},
			MissingInfo: []string{"time"},
			NeedsReply:  true,
			Cited:       []Citation{{MessageID: "m1", Created: "2026-11-01T10:00:00Z", Text: "earlier answer"}},
			Mode:        ModeBare,
			CreatedAt:   time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
		},
		SnoozeUntil: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC),
	}
}

func readFile(t *testing.T, path string) storeFile {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test temp path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return f
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDefaultStorePath(t *testing.T) {
	p, err := DefaultStorePath()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	if filepath.Base(p) != "replydesk.json" || filepath.Base(filepath.Dir(p)) != "webex-mcp" {
		t.Errorf("DefaultStorePath() = %q, want .../webex-mcp/replydesk.json", p)
	}
}

func TestOpenStore_InMemory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, owner := range []string{"", testOwner} {
		s := mustOpen(t, "", owner)
		if err := s.Put(sampleRecord("r1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Get("r1"); !ok {
			t.Fatal("Get after Put: not found")
		}
		if n, err := s.Prune(time.Now().Add(time.Hour)); err != nil || n != 1 {
			t.Fatalf("Prune = %d, %v; want 1, nil", n, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory store touched disk: %v", entries)
	}
}

func TestOpenStore_DiskRequiresOwner(t *testing.T) {
	if _, err := OpenStore(storePath(t), ""); err == nil {
		t.Fatal("OpenStore with empty owner: want error")
	}
}

func TestOpenStore_MissingFileStartsEmpty(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if got := s.All(); len(got) != 0 {
		t.Fatalf("All() = %v, want empty", got)
	}
	if !exists(filepath.Dir(path)) {
		t.Error("store dir not created")
	}
	if exists(path) {
		t.Error("opening created the store file before any Put")
	}
}

func TestStore_RoundTrip(t *testing.T) {
	path := storePath(t)
	clk := &fixedClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
	s := mustOpen(t, path, testOwner)
	s.now = clk.now

	want := []Record{sampleRecord("room-b"), sampleRecord("room-a"), {RoomID: "room-c", HeadID: "h", State: StateDismissed, DismissReason: DismissReacted}}
	for _, r := range want {
		if err := s.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.RoomID, err)
		}
	}

	mustClose(t, s)
	f := readFile(t, path)
	if f.Version != 1 || f.Owner != testOwner || len(f.Records) != 3 {
		t.Fatalf("file = version %d owner %q %d records; want 1, %q, 3", f.Version, f.Owner, len(f.Records), testOwner)
	}
	if tmps := tmpFiles(t, path); len(tmps) != 0 {
		t.Errorf("tmp files left behind: %v", tmps)
	}

	s2 := mustOpen(t, path, testOwner)
	got := s2.All()
	if len(got) != 3 {
		t.Fatalf("reloaded %d records, want 3", len(got))
	}
	for i, id := range []string{"room-a", "room-b", "room-c"} {
		if got[i].RoomID != id {
			t.Errorf("All()[%d].RoomID = %q, want %q (sorted)", i, got[i].RoomID, id)
		}
		if !got[i].UpdatedAt.Equal(clk.now()) {
			t.Errorf("%s UpdatedAt = %v, want %v", id, got[i].UpdatedAt, clk.now())
		}
	}
	a, ok := s2.Get("room-a")
	if !ok || a.Draft == nil {
		t.Fatalf("Get(room-a) = %+v, %v", a, ok)
	}
	src := sampleRecord("room-a")
	if a.Draft.Text != src.Draft.Text || a.Draft.Cited[0].Text != "earlier answer" ||
		a.Draft.Asks[0] != "meet?" || !a.SnoozeUntil.Equal(src.SnoozeUntil) || a.State != StateDrafted {
		t.Errorf("round trip mismatch: %+v / %+v", a, a.Draft)
	}
	c, _ := s2.Get("room-c")
	if c.Draft != nil || c.DismissReason != DismissReacted {
		t.Errorf("room-c = %+v", c)
	}
}

func TestStore_PutUpdatesExisting(t *testing.T) {
	path := storePath(t)
	clk := &fixedClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	s := mustOpen(t, path, testOwner)
	s.now = clk.now

	r := sampleRecord("r")
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	clk.set(clk.now().Add(time.Hour))
	r.State = StateSent
	r.SentMessageID = "sent-1"
	r.UpdatedAt = time.Time{} // ignored; Put sets it
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	got, _ := reopen(t, s, testOwner).Get("r")
	if got.State != StateSent || got.SentMessageID != "sent-1" || !got.UpdatedAt.Equal(clk.now()) {
		t.Errorf("got %+v", got)
	}
}

func TestStore_PutEmptyRoomID(t *testing.T) {
	for _, path := range []string{"", storePath(t)} {
		s := mustOpen(t, path, testOwner)
		if err := s.Put(Record{HeadID: "h"}); err == nil {
			t.Errorf("path %q: Put without RoomID: want error", path)
		}
		if n := len(s.All()); n != 0 {
			t.Errorf("path %q: %d records after rejected Put", path, n)
		}
	}
}

func TestStore_CopiesIsolateState(t *testing.T) {
	s := mustOpen(t, "", "")
	r := sampleRecord("r")
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's record after Put must not change the store.
	r.Draft.Text = "mutated"
	r.Draft.Cited[0].Text = "mutated"
	got, _ := s.Get("r")
	if got.Draft.Text == "mutated" || got.Draft.Cited[0].Text == "mutated" {
		t.Fatal("Put kept a reference to the caller's draft")
	}
	// Mutating a returned record must not change the store either.
	got.Draft.Asks[0] = "mutated"
	all := s.All()
	all[0].Draft.MissingInfo[0] = "mutated"
	again, _ := s.Get("r")
	if again.Draft.Asks[0] == "mutated" || again.Draft.MissingInfo[0] == "mutated" {
		t.Fatal("Get/All returned aliased state")
	}
}

func TestOpenStore_OwnerMismatchDiscards(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, "person-a")
	if err := s.Put(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}

	s2 := reopen(t, s, "person-b")
	if n := len(s2.All()); n != 0 {
		t.Fatalf("owner mismatch kept %d records", n)
	}
	if exists(path) {
		t.Error("store file for another owner was not removed")
	}
	if err := s2.PutSync(sampleRecord("r2")); err != nil {
		t.Fatal(err)
	}
	if f := readFile(t, path); f.Owner != "person-b" || len(f.Records) != 1 {
		t.Errorf("file after new owner Put = %+v", f)
	}
	// The original owner now sees a file that is not theirs.
	if n := len(reopen(t, s2, "person-a").All()); n != 0 {
		t.Errorf("person-a sees %d records from person-b", n)
	}
}

func TestOpenStore_UnreadableMovedAside(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"truncated json", `{"version":1,"owner":"person-owner","records":{`},
		{"not json", "garbage\x00\x01"},
		{"wrong type", `{"version":"one"}`},
		{"unknown version", `{"version":2,"owner":"person-owner","records":{}}`},
		{"missing version", `{"owner":"person-owner","records":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := storePath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := mustOpen(t, path, testOwner)
			if n := len(s.All()); n != 0 {
				t.Errorf("%d records from unreadable file", n)
			}
			if exists(path) {
				t.Error("unreadable file left in place")
			}
			data, err := os.ReadFile(path + ".corrupt") // #nosec G304 -- test temp path
			if err != nil {
				t.Fatalf("corrupt copy: %v", err)
			}
			if string(data) != tt.content {
				t.Errorf("corrupt copy = %q, want %q", data, tt.content)
			}
			if err := s.PutSync(sampleRecord("r")); err != nil {
				t.Fatalf("Put after recovery: %v", err)
			}
			if f := readFile(t, path); len(f.Records) != 1 {
				t.Errorf("records after recovery = %d", len(f.Records))
			}
		})
	}
}

func TestOpenStore_SkipsBadRecordKeys(t *testing.T) {
	path := storePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"version":1,"owner":"person-owner","records":{
		"":       {"room_id":"x","state":"pending"},
		"r1":     {"room_id":"","state":"pending"},
		"r2":     {"room_id":"other","state":"pending"},
		"r3":     {"room_id":"r3","state":"sent"}
	}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := mustOpen(t, path, testOwner).All()
	if len(got) != 2 || got[0].RoomID != "r1" || got[1].RoomID != "r3" {
		t.Errorf("All() = %+v, want r1 (room id filled from key) and r3", got)
	}
}

func TestOpenStore_OversizeFile(t *testing.T) {
	path := storePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxStoreSize+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, testOwner); err == nil {
		t.Fatal("oversize store: want error")
	}
	if !exists(path) {
		t.Error("oversize store was removed; it should be left for the user")
	}
	if exists(filepath.Join(filepath.Dir(path), lockFileName)) {
		t.Error("failed OpenStore left its instance lock behind")
	}
}

func TestOpenStore_ExactlyAtLimit(t *testing.T) {
	path := storePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"owner":"person-owner","records":{}}`
	data := []byte(body + strings.Repeat(" ", maxStoreSize-len(body)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path, testOwner)
	if err != nil {
		t.Fatalf("store at the size limit: %v", err)
	}
	mustClose(t, s)
}

func TestCapBytes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short", "abc", 5, "abc"},
		{"exact", "abcde", 5, "abcde"},
		{"ascii cut", "abcdef", 5, "abcde"},
		{"empty", "", 5, ""},
		{"zero", "abc", 0, ""},
		{"two-byte rune at boundary", "abcdé", 5, "abcd"},         // é is 2 bytes at 4..5
		{"three-byte rune split", "ab€", 4, "ab"},                 // € is 3 bytes at 2..4
		{"three-byte rune fits", "ab€", 5, "ab€"},                 // exactly 5 bytes
		{"four-byte rune split", "a\U0001F600b", 3, "a"},          // emoji at 1..4
		{"four-byte rune fits", "a\U0001F600b", 5, "a\U0001F600"}, // cut after emoji
		{"all multibyte", strings.Repeat("é", 4), 7, "ééé"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capBytes(tt.in, tt.n)
			if got != tt.want {
				t.Errorf("capBytes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
			if len(got) > tt.n {
				t.Errorf("len = %d > %d", len(got), tt.n)
			}
			if utf8.ValidString(tt.in) && !utf8.ValidString(got) {
				t.Errorf("result %q is not valid UTF-8", got)
			}
		})
	}
}

func TestStore_PutCapsCitations(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantLen int
	}{
		{"short kept", "hello", 5},
		{"ascii at limit", strings.Repeat("a", MaxCitationLen), MaxCitationLen},
		{"ascii over", strings.Repeat("a", MaxCitationLen+100), MaxCitationLen},
		// 499 ASCII bytes + a 2-byte rune would straddle 500: drop the rune.
		{"two-byte straddle", strings.Repeat("a", MaxCitationLen-1) + "étail", MaxCitationLen - 1},
		// 498 + 3-byte euro straddles; drop it.
		{"three-byte straddle", strings.Repeat("a", MaxCitationLen-2) + "€", MaxCitationLen - 2},
		// 497 + 3-byte euro fits exactly.
		{"three-byte fits", strings.Repeat("a", MaxCitationLen-3) + "€more", MaxCitationLen},
		{"emoji run", strings.Repeat("\U0001F600", 200), MaxCitationLen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := storePath(t)
			s := mustOpen(t, path, testOwner)
			r := sampleRecord("r")
			r.Draft.Cited = []Citation{{MessageID: "m1", Text: tt.text}, {MessageID: "m2", Text: "ok"}}
			if err := s.Put(r); err != nil {
				t.Fatal(err)
			}
			inMemory := mustGet(t, s, "r")
			for _, got := range []Record{inMemory, mustGet(t, reopen(t, s, testOwner), "r")} {
				c := got.Draft.Cited[0].Text
				if len(c) != tt.wantLen {
					t.Errorf("len = %d, want %d", len(c), tt.wantLen)
				}
				if !utf8.ValidString(c) {
					t.Errorf("capped citation is not valid UTF-8")
				}
				if !strings.HasPrefix(tt.text, c) {
					t.Errorf("capped citation is not a prefix of the original")
				}
				if got.Draft.Cited[1].Text != "ok" {
					t.Errorf("second citation = %q", got.Draft.Cited[1].Text)
				}
			}
			if r.Draft.Cited[0].Text != tt.text {
				t.Error("Put modified the caller's citation")
			}
		})
	}
}

func mustGet(t *testing.T, s *Store, room string) Record {
	t.Helper()
	r, ok := s.Get(room)
	if !ok {
		t.Fatalf("Get(%q): not found", room)
	}
	return r
}

func TestStore_Prune(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		cutoff    time.Time
		wantN     int
		wantRooms []string
	}{
		{"nothing old", base.Add(-time.Hour), 0, []string{"r0", "r1", "r2"}},
		{"cutoff equal to oldest keeps it", base, 0, []string{"r0", "r1", "r2"}},
		{"one old", base.Add(time.Minute), 1, []string{"r1", "r2"}},
		{"two old", base.Add(24*time.Hour + time.Minute), 2, []string{"r2"}},
		{"all old", base.Add(30 * 24 * time.Hour), 3, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := storePath(t)
			clk := &fixedClock{}
			s := mustOpen(t, path, testOwner)
			s.now = clk.now
			for i := range 3 {
				clk.set(base.Add(time.Duration(i) * 24 * time.Hour))
				if err := s.PutSync(Record{RoomID: fmt.Sprintf("r%d", i), State: StatePending}); err != nil {
					t.Fatal(err)
				}
			}
			// Make an unchanged rewrite detectable.
			if err := os.Chtimes(path, base, base); err != nil {
				t.Fatal(err)
			}

			n, err := s.Prune(tt.cutoff)
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if n != tt.wantN {
				t.Errorf("Prune = %d, want %d", n, tt.wantN)
			}
			check := func(label string, recs []Record) {
				var ids []string
				for _, r := range recs {
					ids = append(ids, r.RoomID)
				}
				if strings.Join(ids, ",") != strings.Join(tt.wantRooms, ",") {
					t.Errorf("%s rooms = %v, want %v", label, ids, tt.wantRooms)
				}
			}
			check("memory", s.All())
			mustClose(t, s)

			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := !after.ModTime().Equal(base)
			if rewritten != (tt.wantN > 0) {
				t.Errorf("file rewritten = %v, want %v", rewritten, tt.wantN > 0)
			}
			check("disk", mustOpen(t, path, testOwner).All())
		})
	}
}

func TestPurgePath(t *testing.T) {
	suffixes := []string{"", ".tmp", ".corrupt", ".tmp-123456", ".tmp-abc"}
	for mask := range 1 << len(suffixes) {
		t.Run(fmt.Sprintf("present=%05b", mask), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "replydesk.json")
			for i, suf := range suffixes {
				if mask&(1<<i) != 0 {
					if err := os.WriteFile(path+suf, []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			other := filepath.Join(filepath.Dir(path), "pending.json")
			if err := os.WriteFile(other, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(filepath.Dir(path), lockFileName)
			if err := os.WriteFile(lock, []byte("pid=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			if err := purgePath(path); err != nil {
				t.Fatalf("purgePath: %v", err)
			}
			for _, suf := range suffixes {
				if exists(path + suf) {
					t.Errorf("%s still exists", filepath.Base(path+suf))
				}
			}
			if !exists(other) {
				t.Error("purge removed an unrelated file")
			}
			if !exists(lock) {
				t.Error("purge removed the instance lock; only its holder may")
			}
			if !purgedSince(filepath.Join(filepath.Dir(path), tombstoneFileName), before) {
				t.Error("purge did not leave a current purge marker")
			}
		})
	}
}

func TestPurge_DefaultPath(t *testing.T) {
	cfg := t.TempDir()
	// Point os.UserConfigDir at the temp dir on every platform.
	t.Setenv("AppData", cfg)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	path, err := DefaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, cfg) {
		t.Skipf("could not redirect user config dir (got %s)", path)
	}
	s := mustOpen(t, path, testOwner)
	if err := s.PutSync(sampleRecord("r")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".corrupt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if exists(path) || exists(path+".corrupt") {
		t.Error("Purge left files behind")
	}
	// The open store notices and does not write the state back.
	mustClose(t, s)
	if exists(path) {
		t.Error("closing a store after Purge rewrote the file")
	}
	if err := Purge(); err != nil {
		t.Errorf("second Purge on missing files: %v", err)
	}
}

func TestStore_FileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply; the ACL is set by auth.RestrictFileAccess")
	}
	path := storePath(t)
	// A stale tmp with loose permissions must not leak its mode.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("stale"), 0o644); err != nil { // #nosec G306 -- deliberately loose for the test
		t.Fatal(err)
	}
	s := mustOpen(t, path, testOwner)
	if err := s.PutSync(sampleRecord("r")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("store mode = %o, want 600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("store dir mode = %o, want 700", got)
	}
	for _, name := range []string{lockFileName, tombstoneFileName} {
		if name == tombstoneFileName {
			if err := purgePath(path); err != nil {
				t.Fatal(err)
			}
		}
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, got)
		}
	}
}

func TestStore_PersistFailureKeepsMemory(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	// Make the destination a non-empty directory so the rename fails.
	if err := os.MkdirAll(filepath.Join(path, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSync(sampleRecord("r")); err == nil {
		t.Fatal("PutSync onto a directory: want error")
	}
	if tmps := tmpFiles(t, path); len(tmps) != 0 {
		t.Errorf("tmp files left after failed persist: %v", tmps)
	}
	if _, ok := s.Get("r"); !ok {
		t.Error("record dropped from memory after a failed write")
	}
	if s.LastError() == nil {
		t.Error("LastError() = nil after a failed write")
	}
}

func TestStore_ConcurrentPut(t *testing.T) {
	for _, path := range []string{"", storePath(t)} {
		t.Run(fmt.Sprintf("disk=%v", path != ""), func(t *testing.T) {
			s := mustOpen(t, path, testOwner)
			workers, per := 8, 10
			var wg sync.WaitGroup
			errs := make(chan error, workers*per)
			for w := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range per {
						r := sampleRecord(fmt.Sprintf("w%d-r%d", w, i))
						if err := s.Put(r); err != nil {
							errs <- err
						}
						_, _ = s.Get(r.RoomID)
						_ = s.All()
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("concurrent Put: %v", err)
			}
			if n := len(s.All()); n != workers*per {
				t.Errorf("All() has %d records, want %d", n, workers*per)
			}
			if path != "" {
				if n := len(reopen(t, s, testOwner).All()); n != workers*per {
					t.Errorf("disk has %d records, want %d", n, workers*per)
				}
			}
		})
	}
}

// ---- purge marker (F18) ----

func TestStore_PurgeDisablesPersistence(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if s.Disabled() {
		t.Fatal("new store is disabled")
	}
	if err := s.PutSync(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}

	// --logout in another process.
	if err := purgePath(path); err != nil {
		t.Fatal(err)
	}
	if exists(path) {
		t.Fatal("purge left the store file")
	}

	// A change made after the purge must not bring the state back.
	_ = s.Put(sampleRecord("r2"))
	mustClose(t, s)
	if exists(path) {
		t.Fatal("store rewrote state after it was purged")
	}
	if !s.Disabled() {
		t.Error("Disabled() = false after a purge")
	}
	if n := len(s.All()); n != 0 {
		t.Errorf("disabled store still holds %d records", n)
	}
	if _, ok := s.Get("r1"); ok {
		t.Error("disabled store still returns a purged record")
	}
	if !exists(filepath.Join(filepath.Dir(path), tombstoneFileName)) {
		t.Error("disabled store deleted the purge marker")
	}
}

func TestStore_PurgeDetectedBeforeWrite(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if err := s.PutSync(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}
	if err := purgePath(path); err != nil {
		t.Fatal(err)
	}
	// Disabled notices the marker without waiting for a write.
	if !s.Disabled() {
		t.Fatal("Disabled() = false after a purge")
	}
	if err := s.PutSync(sampleRecord("r2")); !errors.Is(err, ErrPurged) {
		t.Errorf("PutSync after purge = %v, want ErrPurged", err)
	}
	if err := s.Put(sampleRecord("r3")); !errors.Is(err, ErrPurged) {
		t.Errorf("Put after purge = %v, want ErrPurged", err)
	}
	if _, err := s.Prune(time.Now()); !errors.Is(err, ErrPurged) {
		t.Errorf("Prune after purge = %v, want ErrPurged", err)
	}
	if n := len(s.All()); n != 0 {
		t.Errorf("disabled store holds %d records", n)
	}
	if exists(path) {
		t.Error("disabled store wrote the file")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close of a purged store: %v", err)
	}
}

func TestOpenStore_RemovesStaleTombstone(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if err := s.PutSync(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}
	mustClose(t, s)
	if err := purgePath(path); err != nil {
		t.Fatal(err)
	}
	tomb := filepath.Join(filepath.Dir(path), tombstoneFileName)
	if !exists(tomb) {
		t.Fatal("no purge marker")
	}

	// A --review started after --logout begins clean and saves again.
	s2 := mustOpen(t, path, testOwner)
	if exists(tomb) {
		t.Error("OpenStore left the old purge marker")
	}
	if s2.Disabled() {
		t.Fatal("store opened after a purge is disabled")
	}
	if n := len(s2.All()); n != 0 {
		t.Errorf("store after purge has %d records", n)
	}
	if err := s2.PutSync(sampleRecord("r2")); err != nil {
		t.Fatal(err)
	}
	if !fileHasRoom(path, "r2") {
		t.Error("store opened after a purge did not save")
	}
}

func TestPurgedSince(t *testing.T) {
	dir := t.TempDir()
	tomb := filepath.Join(dir, tombstoneFileName)
	opened := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if purgedSince(tomb, opened) {
		t.Error("missing marker counts as purged")
	}
	tests := []struct {
		content string
		want    bool
	}{
		{opened.Add(-time.Second).Format(time.RFC3339Nano), false},
		{opened.Format(time.RFC3339Nano), true},
		{opened.Add(time.Nanosecond).Format(time.RFC3339Nano) + "\n", true},
		{"garbage", true}, // OpenStore removed older markers, so this one is new
	}
	for _, tt := range tests {
		if err := os.WriteFile(tomb, []byte(tt.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := purgedSince(tomb, opened); got != tt.want {
			t.Errorf("purgedSince(%q) = %v, want %v", tt.content, got, tt.want)
		}
	}
}

// ---- instance lock (F28) ----

func TestOpenStore_AlreadyRunning(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)

	_, err := OpenStore(path, testOwner)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second OpenStore = %v, want ErrAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("error %q does not name the holder's pid", err)
	}
	// A different store file in the same dir is the same instance.
	if _, err := OpenStore(filepath.Join(filepath.Dir(path), "other.json"), testOwner); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("OpenStore beside a held lock = %v, want ErrAlreadyRunning", err)
	}
	// The in-memory store needs no lock.
	if _, err := OpenStore("", testOwner); err != nil {
		t.Errorf("in-memory OpenStore while locked: %v", err)
	}

	mustClose(t, s)
	mustOpen(t, path, testOwner)
}

func TestOpenStore_StaleLockTakeover(t *testing.T) {
	tests := []struct {
		name     string
		age      time.Duration
		wantTake bool
	}{
		{"fresh", 30 * time.Second, false},
		{"just under stale", 2*time.Minute + 50*time.Second, false},
		{"stale", 4 * time.Minute, true},
		{"long dead", 48 * time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := storePath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(filepath.Dir(path), lockFileName)
			foreign := []byte("pid=999999\ntime=2026-11-01T00:00:00Z\nnonce=00\n")
			if err := os.WriteFile(lock, foreign, 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-tt.age)
			if err := os.Chtimes(lock, old, old); err != nil {
				t.Fatal(err)
			}

			s, err := OpenStore(path, testOwner)
			if !tt.wantTake {
				if !errors.Is(err, ErrAlreadyRunning) {
					t.Fatalf("OpenStore = %v, want ErrAlreadyRunning", err)
				}
				if !strings.Contains(err.Error(), "pid 999999") {
					t.Errorf("error %q does not name the holder", err)
				}
				data, rerr := os.ReadFile(lock) // #nosec G304 -- test temp path
				if rerr != nil || string(data) != string(foreign) {
					t.Errorf("a fresh lock was disturbed: %q, %v", data, rerr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenStore over a stale lock: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			data, err := os.ReadFile(lock) // #nosec G304 -- test temp path
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), fmt.Sprintf("pid=%d\n", os.Getpid())) {
				t.Errorf("lock after takeover = %q, want our pid", data)
			}
		})
	}
}

func TestStore_CloseReleasesLock(t *testing.T) {
	path := storePath(t)
	lock := filepath.Join(filepath.Dir(path), lockFileName)
	s := mustOpen(t, path, testOwner)
	if !exists(lock) {
		t.Fatal("OpenStore did not create the lock")
	}
	mustClose(t, s)
	if exists(lock) {
		t.Error("Close left the lock")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := s.Put(sampleRecord("r")); err == nil {
		t.Error("Put after Close: want error")
	}

	// A lock another process took over is not ours to remove.
	s2 := mustOpen(t, path, testOwner)
	if err := os.WriteFile(lock, []byte("pid=999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustClose(t, s2)
	if !exists(lock) {
		t.Error("Close removed a lock it no longer owned")
	}
}

func TestStore_LockHeartbeat(t *testing.T) {
	setVar(t, &lockHeartbeat, 10*time.Millisecond)
	path := storePath(t)
	lock := filepath.Join(filepath.Dir(path), lockFileName)
	mustOpen(t, path, testOwner)

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lock mtime refresh", func() bool {
		fi, err := os.Stat(lock)
		return err == nil && time.Since(fi.ModTime()) < time.Minute
	})
	// Refreshed, so a second instance still sees it as held.
	if _, err := OpenStore(path, testOwner); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("OpenStore after heartbeat = %v, want ErrAlreadyRunning", err)
	}
}

func TestStore_TempFilesAreUnique(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	var mu sync.Mutex
	var tmps []string
	s.rename = func(oldpath, newpath string) error {
		mu.Lock()
		tmps = append(tmps, oldpath)
		mu.Unlock()
		return os.Rename(oldpath, newpath)
	}
	for i := range 3 {
		if err := s.PutSync(sampleRecord(fmt.Sprintf("r%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, p := range tmps {
		if filepath.Dir(p) != filepath.Dir(path) {
			t.Errorf("tmp %s is not in the store dir", p)
		}
		base := filepath.Base(p)
		if !strings.HasPrefix(base, "replydesk.json.tmp-") || base == "replydesk.json.tmp-" {
			t.Errorf("tmp name %q does not follow the CreateTemp pattern", base)
		}
		if seen[p] {
			t.Errorf("tmp name %q reused", base)
		}
		seen[p] = true
	}
	if len(tmps) != 3 {
		t.Errorf("%d renames, want 3", len(tmps))
	}
}

// ---- background writes (F29) ----

func TestStore_PutIsAsyncAndCoalesced(t *testing.T) {
	setVar(t, &storeFlushInterval, time.Hour)
	path := storePath(t)
	s := mustOpen(t, path, testOwner)

	// The first write after idle goes out at once, in the background.
	if err := s.Put(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "r1 on disk", func() bool { return fileHasRoom(path, "r1") })

	// Later ones wait for the flush interval: Put itself does not write.
	for i := range 5 {
		if err := s.Put(sampleRecord(fmt.Sprintf("later-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if fileHasRoom(path, "later-0") {
		t.Fatal("Put wrote to disk synchronously instead of scheduling a write")
	}
	if _, ok := s.Get("later-4"); !ok {
		t.Fatal("Put did not update memory")
	}

	// Close flushes the latest state.
	mustClose(t, s)
	for i := range 5 {
		if room := fmt.Sprintf("later-%d", i); !fileHasRoom(path, room) {
			t.Errorf("%s not on disk after Close", room)
		}
	}
}

func TestStore_PutEventuallyOnDisk(t *testing.T) {
	setVar(t, &storeFlushInterval, 20*time.Millisecond)
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	for i := range 4 {
		if err := s.Put(sampleRecord(fmt.Sprintf("r%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "all records on disk", func() bool {
		return fileHasRoom(path, "r0") && fileHasRoom(path, "r3")
	})
	if tmps := tmpFiles(t, path); len(tmps) != 0 {
		t.Errorf("tmp files left: %v", tmps)
	}
}

func TestStore_PutSyncOnDiskImmediately(t *testing.T) {
	setVar(t, &storeFlushInterval, time.Hour)
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if err := s.Put(sampleRecord("async")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first write", func() bool { return fileHasRoom(path, "async") })
	// The writer now holds off for an hour; PutSync must not wait for it.
	r := sampleRecord("sync")
	r.State = StateSending
	if err := s.PutSync(r); err != nil {
		t.Fatal(err)
	}
	f := readFile(t, path)
	if got, ok := f.Records["sync"]; !ok || got.State != StateSending {
		t.Fatalf("PutSync record on disk = %+v, %v", got, ok)
	}
}

func TestStore_LastError(t *testing.T) {
	setVar(t, &storeFlushInterval, time.Millisecond)
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if err := s.LastError(); err != nil {
		t.Fatalf("LastError on a new store = %v", err)
	}
	// A non-empty directory at the store path makes the rename fail.
	if err := os.MkdirAll(filepath.Join(path, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(sampleRecord("r1")); err != nil {
		t.Fatalf("first Put returned %v before any write ran", err)
	}
	eventually(t, "background write error", func() bool { return s.LastError() != nil })
	if !strings.Contains(s.LastError().Error(), "replacing store") {
		t.Errorf("LastError = %v", s.LastError())
	}
	// Put reports the outstanding failure but still keeps the record.
	if err := s.Put(sampleRecord("r2")); err == nil {
		t.Error("Put after a failed background write returned nil")
	}
	if _, ok := s.Get("r2"); !ok {
		t.Error("record dropped after a failed write")
	}

	// Once the path is usable again the next write clears the error.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSync(sampleRecord("r3")); err != nil {
		t.Fatalf("PutSync after recovery: %v", err)
	}
	if err := s.LastError(); err != nil {
		t.Errorf("LastError after a good write = %v", err)
	}
	for _, room := range []string{"r1", "r2", "r3"} {
		if !fileHasRoom(path, room) {
			t.Errorf("%s not on disk after recovery", room)
		}
	}
}

func TestStore_CloseReportsFlushError(t *testing.T) {
	setVar(t, &storeFlushInterval, time.Hour)
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	if err := s.Put(sampleRecord("r1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first write", func() bool { return fileHasRoom(path, "r1") })
	// The writer is idle (holding off for an hour), so swapping rename
	// here does not race it.
	s.writeMu.Lock()
	s.rename = func(string, string) error { return errors.New("disk full") }
	s.writeMu.Unlock()
	if err := s.Put(sampleRecord("r2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Close with a failing flush = %v, want the write error", err)
	}
	if exists(filepath.Join(filepath.Dir(path), lockFileName)) {
		t.Error("Close kept the lock after a failed flush")
	}
}

// ---- rename retry ----

func TestRenameRetry(t *testing.T) {
	denied := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: os.ErrPermission}
	tests := []struct {
		name      string
		failures  int
		failErr   error
		tries     int
		wantCalls int
		wantErr   bool
	}{
		{"first try", 0, denied, 5, 1, false},
		{"transient", 3, denied, 5, 4, false},
		{"last try", 4, denied, 5, 5, false},
		{"persistent", 10, denied, 5, 5, true},
		{"missing source not retried", 10, &os.LinkError{Op: "rename", Err: os.ErrNotExist}, 5, 1, true},
		{"single try", 10, denied, 1, 1, true},
		{"zero tries means one", 10, denied, 0, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			fake := func(oldpath, newpath string) error {
				calls++
				if oldpath != "old" || newpath != "new" {
					t.Errorf("rename(%q, %q)", oldpath, newpath)
				}
				if calls <= tt.failures {
					return tt.failErr
				}
				return nil
			}
			err := renameRetry(fake, tt.tries, time.Microsecond, "old", "new")
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, tt.failErr) {
				t.Errorf("err = %v, want the rename error", err)
			}
			if calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestStore_RenameRetriedOnWrite(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	fails := 2
	flaky := func(oldpath, newpath string) error {
		if fails > 0 {
			fails--
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrPermission}
		}
		return os.Rename(oldpath, newpath)
	}
	s.rename = func(oldpath, newpath string) error {
		return renameRetry(flaky, 5, time.Millisecond, oldpath, newpath)
	}
	if err := s.PutSync(sampleRecord("r")); err != nil {
		t.Fatalf("PutSync with a transiently failing rename: %v", err)
	}
	if fails != 0 {
		t.Errorf("rename failures left = %d; the store did not go through s.rename", fails)
	}
	if !fileHasRoom(path, "r") {
		t.Error("record not on disk")
	}
	if tmps := tmpFiles(t, path); len(tmps) != 0 {
		t.Errorf("tmp files left: %v", tmps)
	}
}

func TestDefaultRenameTries(t *testing.T) {
	want := 1
	if runtime.GOOS == "windows" {
		want = 5
	}
	if got := defaultRenameTries(); got != want {
		t.Errorf("defaultRenameTries() = %d, want %d", got, want)
	}
}
