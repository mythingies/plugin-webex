package replydesk

// The read-receipt invariant, end to end: Reply Desk reads Webex only with
// GETs to an allowlisted set of endpoints, never touches read state
// (memberships, lastSeen, acknowledge, markAsRead), and issues exactly one
// POST /messages, only when the user sends.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
	"github.com/mythingies/plugin-webex/internal/webex"
)

const invariantToken = "invariant-test-token"

type seenRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// webexEmulator is an httptest server for the Webex endpoints Reply Desk
// uses. It records every request.
type webexEmulator struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []seenRequest
	history map[string][]webex.Message // newest first
	posted  []map[string]string
}

func (e *webexEmulator) base() string { return e.srv.URL + "/v1" }

func (e *webexEmulator) requests() []seenRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.reqs)
}

func newWebexEmulator(t *testing.T, now time.Time) *webexEmulator {
	t.Helper()
	at := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339Nano) }
	msg := func(id, room, person, text string, ago time.Duration) webex.Message {
		email := "alice@example.com"
		if person == meID {
			email = "me@example.com"
		}
		return webex.Message{ID: id, RoomID: room, RoomType: "direct", PersonID: person, PersonEmail: email, Text: text, Created: at(ago)}
	}

	// room-a: Alice asks in a thread; my last reply was an hour ago.
	a2 := msg("a2", "room-a", otherID, "Where is the staging deploy key kept?", 5*time.Minute)
	a2.ParentID = "a-root"
	a1 := msg("a1", "room-a", otherID, "Also, quick question", 6*time.Minute)
	a1.ParentID = "a-root"
	roomA := []webex.Message{a2, a1, msg("a-root", "room-a", meID, "Shipping the release today", time.Hour)}
	for i := range 80 {
		who, text := otherID, fmt.Sprintf("older chat %d", i)
		if i%3 == 0 {
			who, text = meID, fmt.Sprintf("noted, item %d", i)
		}
		roomA = append(roomA, msg(fmt.Sprintf("a-old-%d", i), "room-a", who, text, 2*time.Hour+time.Duration(i)*time.Hour))
	}
	roomA[20].PersonID, roomA[20].Text = meID, "The staging deploy key is kept in vault under ops/staging"

	e := &webexEmulator{
		t: t,
		history: map[string][]webex.Message{
			"room-a": roomA,
			// room-b: answered (newest is mine).
			"room-b": {msg("b2", "room-b", meID, "done", 10*time.Minute), msg("b1", "room-b", "bob-id", "can you?", 20*time.Minute)},
			// room-c: Carol, no reply from me yet; on the second rooms page.
			"room-c": {msg("c1", "room-c", "carol-id", "hello there", 30*time.Minute)},
		},
	}
	rooms := [][]webex.Space{
		{
			{ID: "room-a", Title: "Alice Example", Type: "direct", LastActivity: at(5 * time.Minute)},
			{ID: "room-b", Title: "Bob Example", Type: "direct", LastActivity: at(10 * time.Minute)},
		},
		{
			{ID: "room-c", Title: "Carol Example", Type: "direct", LastActivity: at(30 * time.Minute)},
		},
	}

	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		e.mu.Lock()
		e.reqs = append(e.reqs, seenRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
		e.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+invariantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/people/me":
			writeJSON(webex.Person{ID: meID, DisplayName: "Me Myself", Emails: []string{"me@example.com"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/rooms":
			page := 0
			if q.Get("cursor") == "2" {
				page = 1
			} else {
				w.Header().Set("Link", fmt.Sprintf(`<%s/v1/rooms?type=direct&sortBy=lastactivity&cursor=2>; rel="next"`, e.srv.URL))
			}
			writeJSON(map[string]any{"items": rooms[page]})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages":
			e.mu.Lock()
			h := slices.Clone(e.history[q.Get("roomId")])
			e.mu.Unlock()
			start := 0
			if b := q.Get("beforeMessage"); b != "" {
				i := slices.IndexFunc(h, func(m webex.Message) bool { return m.ID == b })
				if i < 0 {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				start = i + 1
			}
			n := 50
			if v, err := strconv.Atoi(q.Get("max")); err == nil && v > 0 {
				n = v
			}
			writeJSON(map[string]any{"items": h[start:min(start+n, len(h))]})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
			e.mu.Lock()
			defer e.mu.Unlock()
			for _, h := range e.history {
				for _, m := range h {
					if m.ID == id {
						writeJSON(m)
						return
					}
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			var in map[string]string
			if err := json.Unmarshal(body, &in); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			e.mu.Lock()
			e.posted = append(e.posted, in)
			id := fmt.Sprintf("posted-%d", len(e.posted))
			e.mu.Unlock()
			writeJSON(webex.Message{ID: id, RoomID: in["roomId"], ParentID: in["parentId"], Text: in["text"], PersonID: meID})
		default:
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

var allowedGETPath = regexp.MustCompile(`^/v1/(people/me|rooms|messages|messages/[A-Za-z0-9_-]+)$`)

// checkInvariant asserts the read-receipt invariant over reqs, where
// sendStart is the index of the first request made by Send.
func checkInvariant(t *testing.T, reqs []seenRequest, sendStart int, wantRoom, wantParent, wantText string) {
	t.Helper()
	posts := 0
	for i, r := range reqs {
		all := strings.ToLower(r.Path + "?" + r.Query + " " + r.Body)
		for _, bad := range []string{"acknowledge", "markasread", "lastseen", "/memberships"} {
			if strings.Contains(all, bad) {
				t.Errorf("request %d touches read state (%q): %+v", i, bad, r)
			}
		}
		switch r.Method {
		case http.MethodGet:
			if !allowedGETPath.MatchString(r.Path) {
				t.Errorf("request %d: GET to a path outside the allowlist: %s", i, r.Path)
			}
		case http.MethodPost:
			posts++
			if r.Path != "/v1/messages" {
				t.Errorf("request %d: POST to %s", i, r.Path)
			}
			if i < sendStart {
				t.Errorf("request %d: POST before Send", i)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				t.Fatalf("POST body: %v", err)
			}
			want := map[string]string{"roomId": wantRoom, "text": wantText}
			if wantParent != "" {
				want["parentId"] = wantParent
			}
			if !mapsEqual(body, want) {
				t.Errorf("POST body = %v, want %v", body, want)
			}
		default:
			t.Errorf("request %d: unexpected method %s %s", i, r.Method, r.Path)
		}
	}
	if posts != 1 {
		t.Errorf("POST count = %d, want exactly 1", posts)
	}
	if sendStart <= 0 || sendStart >= len(reqs) {
		t.Fatalf("sendStart %d out of range (%d requests)", sendStart, len(reqs))
	}
	// Coverage: the flow really exercised the paging endpoints.
	var sawRoomsPage2, sawBefore bool
	for _, r := range reqs[:sendStart] {
		sawRoomsPage2 = sawRoomsPage2 || (r.Path == "/v1/rooms" && strings.Contains(r.Query, "cursor=2"))
		sawBefore = sawBefore || (r.Path == "/v1/messages" && strings.Contains(r.Query, "beforeMessage="))
	}
	if !sawRoomsPage2 || !sawBefore {
		t.Errorf("flow did not follow rooms paging (%v) or page messages (%v)", sawRoomsPage2, sawBefore)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func newInvariantService(t *testing.T, e *webexEmulator, d Drafter) *Service {
	t.Helper()
	client := webex.NewClientWithBaseURL(auth.NewStaticProvider(invariantToken), e.base())
	s, err := NewService(Config{API: client, StorePath: "", Drafter: d, PreDraft: -1})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.wait)
	return s
}

func invariantDrafter() *fakeDrafter {
	d := newFakeDrafter()
	d.fn = func(in DraftInput) (*DraftOutput, error) {
		cited := []string{}
		if len(in.Prior) > 0 {
			cited = append(cited, in.Prior[0].Ref)
		}
		return &DraftOutput{NeedsReply: true, Summary: "where the key is", Draft: "It is in vault under ops/staging.", CitedRefs: cited}, nil
	}
	return d
}

func TestReadReceiptInvariant(t *testing.T) {
	logs := captureSlog(t)
	e := newWebexEmulator(t, time.Now())
	d := invariantDrafter()
	s := newInvariantService(t, e, d)

	rescan(s)
	items := s.Items(false)
	if got := itemIDs(items); !slices.Equal(got, []string{"room-a", "room-c"}) {
		t.Fatalf("items = %v, want [room-a room-c]", got)
	}
	if e := s.Draft(context.Background(), "room-a", ""); e != nil {
		t.Fatal(e)
	}
	s.wait()
	it := itemFor(t, s, "room-a")
	if it.State != StateDrafted || it.Draft == nil || len(it.Draft.Cited) != 1 || !strings.Contains(it.Draft.Cited[0].Text, "ops/staging") {
		t.Fatalf("draft = %+v", it.Draft)
	}

	sendStart := len(e.requests())
	text := it.Draft.Text
	id, status, apiErr := s.Send(context.Background(), sendReq("room-a", text))
	if apiErr != nil || status != http.StatusOK || id != "posted-1" {
		t.Fatalf("Send = %q %d %v", id, status, apiErr)
	}
	checkInvariant(t, e.requests(), sendStart, "room-a", "a-root", text)

	out := logs.String()
	for _, secret := range []string{invariantToken, text, "Where is the staging deploy key", "ops/staging"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestReadReceiptInvariantThroughServe(t *testing.T) {
	// Nothing in the review flow may write to stdout (the MCP channel when
	// the binary runs as a server).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	stdoutCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		stdoutCh <- b
	}()
	restored := false
	restore := func() {
		if !restored {
			os.Stdout = orig
			_ = w.Close()
			restored = true
		}
	}
	defer restore()

	e := newWebexEmulator(t, time.Now())
	s := newInvariantService(t, e, invariantDrafter())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := newReviewServer(ln, s, ServeOptions{Log: io.Discard}, serveDeps{open: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rs.run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("review server did not stop")
		}
	}()

	hc := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	waitUp := time.Now().Add(5 * time.Second)
	var cookie *http.Cookie
	for cookie == nil {
		resp, err := hc.Get(rs.launchURL())
		if err == nil {
			for _, c := range resp.Cookies() {
				if c.Name == sessionCookieName {
					cookie = c
				}
			}
			_ = resp.Body.Close()
		}
		if cookie == nil {
			if time.Now().After(waitUp) {
				t.Fatalf("login failed: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	call := func(method, path string, body any, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, rs.origin+path, rd)
		req.AddCookie(cookie)
		if method == http.MethodPost {
			req.Header.Set("Origin", rs.origin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(csrfHeaderName, rs.csrf)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
			}
		}
		return resp.StatusCode
	}
	items := func() []ItemView {
		var v struct {
			Items []ItemView `json:"items"`
		}
		call(http.MethodGet, "/api/items", nil, &v)
		return v.Items
	}
	poll := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// run() starts a scan on startup.
	poll("the startup scan", func() bool {
		var st StatusView
		call(http.MethodGet, "/api/status", nil, &st)
		return !st.Scanning && st.LastScan != ""
	})
	if got := itemIDs(items()); !slices.Equal(got, []string{"room-a", "room-c"}) {
		t.Fatalf("items = %v", got)
	}
	if st := call(http.MethodPost, "/api/draft", map[string]string{"room_id": "room-a", "instruction": ""}, nil); st != http.StatusAccepted {
		t.Fatalf("draft status = %d", st)
	}
	var drafted ItemView
	poll("the draft", func() bool {
		for _, it := range items() {
			if it.RoomID == "room-a" && it.State == StateDrafted {
				drafted = it
				return true
			}
		}
		return false
	})

	sendStart := len(e.requests())
	text := drafted.Draft.Text
	var sent struct {
		MessageID string `json:"message_id"`
	}
	if st := call(http.MethodPost, "/api/send", SendRequest{RoomID: "room-a", Text: text, TextSHA256: sha256Hex(text)}, &sent); st != http.StatusOK || sent.MessageID != "posted-1" {
		t.Fatalf("send = %d %+v", st, sent)
	}
	reqs := e.requests()
	checkInvariant(t, reqs, sendStart, "room-a", "a-root", text)

	// After the send, reading the inbox again must not post anything.
	_ = items()
	call(http.MethodPost, "/api/scan", map[string]string{}, nil)
	s.wait()
	_ = items()
	for _, r := range e.requests()[len(reqs):] {
		if r.Method != http.MethodGet {
			t.Errorf("request after send: %s %s", r.Method, r.Path)
		}
	}

	restore()
	if out := <-stdoutCh; len(out) != 0 {
		t.Fatalf("review flow wrote %d bytes to stdout: %q", len(out), out)
	}
}
