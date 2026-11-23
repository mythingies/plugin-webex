package replydesk

import (
	"encoding/json"
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

func mustOpen(t *testing.T, path, owner string) *Store {
	t.Helper()
	s, err := OpenStore(path, owner)
	if err != nil {
		t.Fatalf("OpenStore(%q, %q): %v", path, owner, err)
	}
	return s
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

	f := readFile(t, path)
	if f.Version != 1 || f.Owner != testOwner || len(f.Records) != 3 {
		t.Fatalf("file = version %d owner %q %d records; want 1, %q, 3", f.Version, f.Owner, len(f.Records), testOwner)
	}
	if exists(path + ".tmp") {
		t.Error("tmp file left behind")
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
	got, _ := mustOpen(t, path, testOwner).Get("r")
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

	s2 := mustOpen(t, path, "person-b")
	if n := len(s2.All()); n != 0 {
		t.Fatalf("owner mismatch kept %d records", n)
	}
	if exists(path) {
		t.Error("store file for another owner was not removed")
	}
	if err := s2.Put(sampleRecord("r2")); err != nil {
		t.Fatal(err)
	}
	if f := readFile(t, path); f.Owner != "person-b" || len(f.Records) != 1 {
		t.Errorf("file after new owner Put = %+v", f)
	}
	// The original owner now sees a file that is not theirs.
	if n := len(mustOpen(t, path, "person-a").All()); n != 0 {
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
			if err := s.Put(sampleRecord("r")); err != nil {
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
	if _, err := OpenStore(path, testOwner); err != nil {
		t.Fatalf("store at the size limit: %v", err)
	}
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
			for _, got := range []Record{mustGet(t, s, "r"), mustGet(t, mustOpen(t, path, testOwner), "r")} {
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
				if err := s.Put(Record{RoomID: fmt.Sprintf("r%d", i), State: StatePending}); err != nil {
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
			check("disk", mustOpen(t, path, testOwner).All())

			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := !after.ModTime().Equal(base)
			if rewritten != (tt.wantN > 0) {
				t.Errorf("file rewritten = %v, want %v", rewritten, tt.wantN > 0)
			}
		})
	}
}

func TestPurgePath(t *testing.T) {
	suffixes := []string{"", ".tmp", ".corrupt"}
	for mask := range 1 << len(suffixes) {
		t.Run(fmt.Sprintf("present=%03b", mask), func(t *testing.T) {
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
	if err := s.Put(sampleRecord("r")); err != nil {
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
	if err := s.Put(sampleRecord("r")); err != nil {
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
}

func TestStore_PersistFailureKeepsMemory(t *testing.T) {
	path := storePath(t)
	s := mustOpen(t, path, testOwner)
	// Make the destination a non-empty directory so the rename fails.
	if err := os.MkdirAll(filepath.Join(path, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(sampleRecord("r")); err == nil {
		t.Fatal("Put onto a directory: want error")
	}
	if exists(path + ".tmp") {
		t.Error("tmp file left after failed persist")
	}
	if _, ok := s.Get("r"); !ok {
		t.Error("record dropped from memory after a failed write")
	}
}

func TestStore_ConcurrentPut(t *testing.T) {
	for _, path := range []string{"", storePath(t)} {
		t.Run(fmt.Sprintf("disk=%v", path != ""), func(t *testing.T) {
			s := mustOpen(t, path, testOwner)
			workers, per := 8, 10
			if path != "" {
				workers, per = 4, 5 // each disk write runs icacls on Windows
			}
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
				if n := len(mustOpen(t, path, testOwner).All()); n != workers*per {
					t.Errorf("disk has %d records, want %d", n, workers*per)
				}
			}
		})
	}
}
