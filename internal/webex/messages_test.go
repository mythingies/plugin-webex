package webex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token"

type staticToken string

func (s staticToken) Token() (string, error) { return string(s), nil }

// recorder is an httptest server that answers each request with the next
// response from handle and records what it received.
type recorder struct {
	mu       sync.Mutex
	requests []*http.Request
	srv      *httptest.Server
}

func newRecorder(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		n := len(rec.requests)
		rec.requests = append(rec.requests, r.Clone(r.Context()))
		rec.mu.Unlock()
		handle(n, w, r)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) req(i int) *http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[i]
}

// client returns a client on the recorder's /v1 base whose sleeps are recorded.
func (r *recorder) client(sleeps *[]time.Duration) *Client {
	c := NewClientWithBaseURL(staticToken(testToken), r.srv.URL+"/v1/")
	c.sleep = func(d time.Duration) {
		if sleeps != nil {
			*sleeps = append(*sleeps, d)
		}
	}
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func roomsPage(rooms ...Space) map[string]any {
	if rooms == nil {
		rooms = []Space{}
	}
	return map[string]any{"items": rooms}
}

func room(id string, last time.Time) Space {
	return Space{ID: id, Type: "direct", LastActivity: last.UTC().Format("2006-01-02T15:04:05.000Z")}
}

func ids(rooms []Space) string {
	out := make([]string, len(rooms))
	for i, r := range rooms {
		out[i] = r.ID
	}
	return strings.Join(out, ",")
}

func checkAuth(t *testing.T, rec *recorder) {
	t.Helper()
	for i := 0; i < rec.count(); i++ {
		if got := rec.req(i).Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("request %d Authorization = %q", i, got)
		}
	}
}

func TestNewClientBaseURL(t *testing.T) {
	if c := NewClient(staticToken("x")); c.baseURL != defaultBaseURL {
		t.Errorf("NewClient baseURL = %q", c.baseURL)
	}
	for _, in := range []string{"http://h/v1", "http://h/v1/", "http://h/v1//"} {
		if c := NewClientWithBaseURL(staticToken("x"), in); c.baseURL != "http://h/v1" {
			t.Errorf("NewClientWithBaseURL(%q) baseURL = %q", in, c.baseURL)
		}
	}
}

func TestExistingMethodsHonorBaseURL(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, Person{ID: "me", DisplayName: "Me"})
	})
	me, err := rec.client(nil).GetMe()
	if err != nil || me.ID != "me" {
		t.Fatalf("GetMe = %+v, %v", me, err)
	}
	if p := rec.req(0).URL.Path; p != "/v1/people/me" {
		t.Errorf("path = %q", p)
	}
	checkAuth(t, rec)
}

func TestListDirectRoomsPaging(t *testing.T) {
	now := time.Now()
	var rec *recorder
	rec = newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/v1/rooms?cursor=p2>; rel="next"`, rec.srv.URL))
			writeJSON(t, w, roomsPage(room("a", now), room("b", now.Add(-time.Minute))))
		case "p2":
			// Several links in one header, next not first.
			w.Header().Set("Link", fmt.Sprintf(`<%[1]s/v1/rooms?cursor=p1>; rel="prev", <%[1]s/v1/rooms?cursor=p3>; rel="next"`, rec.srv.URL))
			writeJSON(t, w, roomsPage(room("c", now.Add(-2*time.Minute))))
		case "p3":
			writeJSON(t, w, roomsPage(room("d", now.Add(-3*time.Minute))))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	})

	rooms, err := rec.client(nil).ListDirectRooms(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rooms); got != "a,b,c,d" {
		t.Errorf("rooms = %s", got)
	}
	if rec.count() != 3 {
		t.Errorf("requests = %d, want 3", rec.count())
	}
	q := rec.req(0).URL.Query()
	if rec.req(0).URL.Path != "/v1/rooms" || q.Get("type") != "direct" || q.Get("sortBy") != "lastactivity" || q.Get("max") != "1000" {
		t.Errorf("first request = %s", rec.req(0).URL)
	}
	checkAuth(t, rec)
}

func TestListDirectRoomsRelativeAndEmptyPages(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		links []string // Link header for page 0, 1, ...; "" ends paging
		pages [][]Space
		want  string
	}{
		{
			name:  "empty page that still has a next link",
			links: []string{`</v1/rooms?cursor=1>; rel="next"`, ""},
			pages: [][]Space{nil, {room("a", now)}},
			want:  "a",
		},
		{
			name:  "several empty pages",
			links: []string{`</v1/rooms?cursor=1>; rel=next`, `</v1/rooms?cursor=2>; rel=next`, ""},
			pages: [][]Space{nil, nil, {room("a", now)}},
			want:  "a",
		},
		{
			name:  "relative reference without a leading slash",
			links: []string{`<rooms?cursor=1>; rel="next"`, ""},
			pages: [][]Space{{room("a", now)}, {room("b", now)}},
			want:  "a,b",
		},
		{
			name:  "rel lists several types",
			links: []string{`</v1/rooms?cursor=1>; title="x, y"; rel="prev next"`, ""},
			pages: [][]Space{{room("a", now)}, {room("b", now)}},
			want:  "a,b",
		},
		{
			name:  "non-direct rooms are dropped",
			links: []string{""},
			pages: [][]Space{{room("a", now), {ID: "g", Type: "group"}, room("b", now)}},
			want:  "a,b",
		},
		{
			name:  "no rooms at all",
			links: []string{""},
			pages: [][]Space{nil},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n >= len(tt.pages) {
					t.Errorf("unexpected request %d: %s", n, r.URL)
					writeJSON(t, w, roomsPage())
					return
				}
				if tt.links[n] != "" {
					w.Header().Set("Link", tt.links[n])
				}
				writeJSON(t, w, roomsPage(tt.pages[n]...))
			})
			rooms, err := rec.client(nil).ListDirectRooms(time.Time{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(rooms); got != tt.want {
				t.Errorf("rooms = %q, want %q", got, tt.want)
			}
			if rec.count() != len(tt.pages) {
				t.Errorf("requests = %d, want %d", rec.count(), len(tt.pages))
			}
			checkAuth(t, rec)
		})
	}
}

func TestListDirectRoomsRefusesForeignNextLink(t *testing.T) {
	now := time.Now()
	other := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, roomsPage(room("stolen", now)))
	})
	tests := []struct {
		name string
		link func(base string) string
	}{
		{"other host", func(string) string { return other.srv.URL + "/v1/rooms?cursor=2" }},
		{"other port", func(base string) string {
			u, _ := url.Parse(base)
			port, _ := strconv.Atoi(u.Port())
			return fmt.Sprintf("http://%s:%d/v1/rooms", u.Hostname(), port+1)
		}},
		{"other scheme", func(base string) string { return strings.Replace(base, "http://", "https://", 1) + "/v1/rooms" }},
		{"scheme-relative other host", func(string) string { return "//" + strings.TrimPrefix(other.srv.URL, "http://") + "/v1/rooms" }},
		{"path outside base", func(base string) string { return base + "/v2/rooms" }},
		{"path sharing the base prefix", func(base string) string { return base + "/v1evil/rooms" }},
		{"dot segments", func(base string) string { return base + "/v1/../admin" }},
		{"encoded dot segments", func(base string) string { return base + "/v1/%2e%2e/admin" }},
		{"encoded slash and dots", func(base string) string { return base + "/v1/rooms%2F..%2F..%2Fadmin" }},
		{"userinfo", func(base string) string { return strings.Replace(base, "http://", "http://user:pw@", 1) + "/v1/rooms" }},
		{"javascript scheme", func(string) string { return "javascript:alert(1)" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec *recorder
			rec = newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n > 0 {
					t.Errorf("followed the link to %s", r.URL)
				}
				w.Header().Set("Link", "<"+tt.link(rec.srv.URL)+`>; rel="next"`)
				writeJSON(t, w, roomsPage(room("a", now)))
			})
			before := other.count()
			rooms, err := rec.client(nil).ListDirectRooms(time.Time{}, 0)
			if !errors.Is(err, errUnsafeNextLink) {
				t.Errorf("err = %v, want errUnsafeNextLink", err)
			}
			if got := ids(rooms); got != "a" {
				t.Errorf("rooms = %q, want the first page", got)
			}
			if rec.count() != 1 || other.count() != before {
				t.Errorf("requests: base %d, other host %d", rec.count(), other.count()-before)
			}
		})
	}
}

func TestListDirectRoomsActiveSinceAndLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	page1 := []Space{
		room("a", now),
		{ID: "bad", Type: "direct", LastActivity: "yesterday"},
		room("b", now.Add(-1*time.Hour)),
	}
	page2 := []Space{
		room("c", now.Add(-2*time.Hour)),
		room("old", now.Add(-48*time.Hour)),
		room("older", now.Add(-72*time.Hour)),
	}
	page3 := []Space{room("ancient", now.Add(-96*time.Hour))}

	tests := []struct {
		name         string
		activeSince  time.Time
		limit        int
		wantRooms    string
		wantRequests int
		wantMax      string
	}{
		{"no cutoff, no limit", time.Time{}, 0, "a,bad,b,c,old,older,ancient", 3, "1000"},
		{"cutoff stops paging", now.Add(-24 * time.Hour), 0, "a,b,c", 2, "1000"},
		{"cutoff inside first page", now.Add(-30 * time.Minute), 0, "a", 1, "1000"},
		{"cutoff in the future", now.Add(time.Hour), 0, "", 1, "1000"},
		{"limit within first page", time.Time{}, 2, "a,bad", 1, "2"},
		{"limit across pages", time.Time{}, 4, "a,bad,b,c", 2, "4"},
		{"limit counts only kept rooms", now.Add(-24 * time.Hour), 3, "a,b,c", 2, "3"},
		{"limit above page max", time.Time{}, 5000, "a,bad,b,c,old,older,ancient", 3, "1000"},
		{"negative limit is no limit", time.Time{}, -1, "a,bad,b,c,old,older,ancient", 3, "1000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
				switch n {
				case 0:
					w.Header().Set("Link", `</v1/rooms?cursor=2>; rel="next"`)
					writeJSON(t, w, roomsPage(page1...))
				case 1:
					w.Header().Set("Link", `</v1/rooms?cursor=3>; rel="next"`)
					writeJSON(t, w, roomsPage(page2...))
				default:
					writeJSON(t, w, roomsPage(page3...))
				}
			})
			rooms, err := rec.client(nil).ListDirectRooms(tt.activeSince, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(rooms); got != tt.wantRooms {
				t.Errorf("rooms = %q, want %q", got, tt.wantRooms)
			}
			if rec.count() != tt.wantRequests {
				t.Errorf("requests = %d, want %d", rec.count(), tt.wantRequests)
			}
			if got := rec.req(0).URL.Query().Get("max"); got != tt.wantMax {
				t.Errorf("max = %q, want %q", got, tt.wantMax)
			}
		})
	}
}

func TestListDirectRoomsPageCap(t *testing.T) {
	rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`</v1/rooms?cursor=%d>; rel="next"`, n+1))
		writeJSON(t, w, roomsPage(room(strconv.Itoa(n), time.Now())))
	})
	rooms, err := rec.client(nil).ListDirectRooms(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.count() != maxRoomsPages || len(rooms) != maxRoomsPages {
		t.Errorf("requests = %d, rooms = %d, want %d", rec.count(), len(rooms), maxRoomsPages)
	}
}

func TestListDirectRoomsErrorKeepsEarlierPages(t *testing.T) {
	rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			w.Header().Set("Link", `</v1/rooms?cursor=2>; rel="next"`)
			writeJSON(t, w, roomsPage(room("a", time.Now())))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	rooms, err := rec.client(nil).ListDirectRooms(time.Time{}, 0)
	if err == nil || err.Error() != "webex API error (status 500)" {
		t.Errorf("err = %v", err)
	}
	if ids(rooms) != "a" {
		t.Errorf("rooms = %q", ids(rooms))
	}
}

func TestListMessagesQuery(t *testing.T) {
	before := time.Date(2026, 11, 20, 10, 30, 0, 0, time.FixedZone("x", 3600))
	tests := []struct {
		name string
		opts ListMessagesOptions
		want url.Values
	}{
		{"room only", ListMessagesOptions{RoomID: "r1"}, url.Values{"roomId": {"r1"}}},
		{"all filters", ListMessagesOptions{RoomID: "r1", ParentID: "p1", BeforeMessage: "m1", Before: before, MentionedMe: true, Max: 20},
			url.Values{"roomId": {"r1"}, "parentId": {"p1"}, "beforeMessage": {"m1"}, "before": {"2026-11-20T09:30:00Z"}, "mentionedPeople": {"me"}, "max": {"20"}}},
		{"max 1", ListMessagesOptions{RoomID: "r1", Max: 1}, url.Values{"roomId": {"r1"}, "max": {"1"}}},
		{"max 1000", ListMessagesOptions{RoomID: "r1", Max: 1000}, url.Values{"roomId": {"r1"}, "max": {"1000"}}},
		{"max clamped to 1000", ListMessagesOptions{RoomID: "r1", Max: 5000}, url.Values{"roomId": {"r1"}, "max": {"1000"}}},
		{"negative max omitted", ListMessagesOptions{RoomID: "r1", Max: -3}, url.Values{"roomId": {"r1"}}},
		{"mentions cap at 100", ListMessagesOptions{RoomID: "r1", MentionedMe: true, Max: 500}, url.Values{"roomId": {"r1"}, "mentionedPeople": {"me"}, "max": {"100"}}},
		{"mentions below cap", ListMessagesOptions{RoomID: "r1", MentionedMe: true, Max: 60}, url.Values{"roomId": {"r1"}, "mentionedPeople": {"me"}, "max": {"60"}}},
		{"mentions default max omitted", ListMessagesOptions{RoomID: "r1", MentionedMe: true}, url.Values{"roomId": {"r1"}, "mentionedPeople": {"me"}}},
		{"room ID is escaped", ListMessagesOptions{RoomID: "a&b=c d"}, url.Values{"roomId": {"a&b=c d"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]any{"items": []Message{{ID: "m2", Text: "hi"}, {ID: "m1"}}})
			})
			msgs, err := rec.client(nil).ListMessages(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 || msgs[0].ID != "m2" || msgs[0].Text != "hi" {
				t.Errorf("messages = %+v", msgs)
			}
			r := rec.req(0)
			if r.Method != http.MethodGet || r.URL.Path != "/v1/messages" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.URL.Query(); got.Encode() != tt.want.Encode() {
				t.Errorf("query = %s, want %s", got.Encode(), tt.want.Encode())
			}
			checkAuth(t, rec)
		})
	}
}

func TestListMessagesRequiresRoom(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {})
	if _, err := rec.client(nil).ListMessages(ListMessagesOptions{ParentID: "p"}); err == nil {
		t.Error("want an error without RoomID")
	}
	if rec.count() != 0 {
		t.Errorf("requests = %d, want 0", rec.count())
	}
}

func TestListMessages504Downshift(t *testing.T) {
	tests := []struct {
		name      string
		opts      ListMessagesOptions
		statuses  []int // per request; anything after is 200
		wantMax   []string
		wantErr   bool
		wantCount int
	}{
		{"halves max", ListMessagesOptions{RoomID: "r", Max: 100}, []int{504}, []string{"100", "50"}, false, 2},
		{"default max halves to 25", ListMessagesOptions{RoomID: "r"}, []int{504}, []string{"", "25"}, false, 2},
		{"halving floors at 10", ListMessagesOptions{RoomID: "r", Max: 15}, []int{504}, []string{"15", "10"}, false, 2},
		{"never grows a small page", ListMessagesOptions{RoomID: "r", Max: 4}, []int{504}, []string{"4", "4"}, false, 2},
		{"from the clamped max", ListMessagesOptions{RoomID: "r", Max: 5000}, []int{504}, []string{"1000", "500"}, false, 2},
		{"retries only once", ListMessagesOptions{RoomID: "r", Max: 100}, []int{504, 504}, []string{"100", "50"}, true, 2},
		{"other 5xx not retried", ListMessagesOptions{RoomID: "r", Max: 100}, []int{502}, []string{"100"}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n < len(tt.statuses) {
					w.WriteHeader(tt.statuses[n])
					return
				}
				writeJSON(t, w, map[string]any{"items": []Message{{ID: "m"}}})
			})
			msgs, err := rec.client(nil).ListMessages(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(msgs) != 1 {
				t.Errorf("messages = %+v", msgs)
			}
			if rec.count() != tt.wantCount {
				t.Fatalf("requests = %d, want %d", rec.count(), tt.wantCount)
			}
			for i, want := range tt.wantMax {
				if got := rec.req(i).URL.Query().Get("max"); got != want {
					t.Errorf("request %d max = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestGetMessage(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		status   int
		wantPath string
		wantErr  error // nil: success
		wantMsg  string
	}{
		{"found", "m1", http.StatusOK, "/v1/messages/m1", nil, ""},
		{"ID is path-escaped", "a/b?c", http.StatusOK, "/v1/messages/a%2Fb%3Fc", nil, ""},
		{"deleted", "gone", http.StatusNotFound, "/v1/messages/gone", ErrNotFound, "webex API error (status 404)"},
		{"server error", "m1", http.StatusInternalServerError, "/v1/messages/m1", nil, "webex API error (status 500)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				if tt.status != http.StatusOK {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(`{"message":"nope"}`))
					return
				}
				writeJSON(t, w, Message{ID: tt.id, Text: "hello"})
			})
			msg, err := rec.client(nil).GetMessage(tt.id)
			if got := rec.req(0).URL.EscapedPath(); got != tt.wantPath {
				t.Errorf("path = %q, want %q", got, tt.wantPath)
			}
			switch {
			case tt.wantMsg != "":
				if err == nil || err.Error() != tt.wantMsg {
					t.Errorf("err = %v, want %q", err, tt.wantMsg)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("errors.Is(%v, %v) = false", err, tt.wantErr)
				}
				if tt.wantErr == nil && errors.Is(err, ErrNotFound) {
					t.Errorf("%v must not match ErrNotFound", err)
				}
			case err != nil:
				t.Fatal(err)
			case msg.ID != tt.id || msg.Text != "hello":
				t.Errorf("message = %+v", msg)
			}
			checkAuth(t, rec)
		})
	}
}

func TestGetMessageRequiresID(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {})
	if _, err := rec.client(nil).GetMessage(""); err == nil {
		t.Error("want an error for an empty ID")
	}
	if rec.count() != 0 {
		t.Errorf("requests = %d", rec.count())
	}
}

func TestGetRateLimit(t *testing.T) {
	type reply struct {
		status     int
		retryAfter string // "-" omits the header
	}
	tests := []struct {
		name       string
		replies    []reply // anything after is 200
		wantSleeps []time.Duration
		wantErr    *RateLimitError
		wantCount  int
	}{
		{"429 short wait retried", []reply{{429, "1"}}, []time.Duration{time.Second}, nil, 2},
		{"423 short wait retried", []reply{{423, "2"}}, []time.Duration{2 * time.Second}, nil, 2},
		{"retried twice", []reply{{429, "1"}, {429, "1"}}, []time.Duration{time.Second, time.Second}, nil, 3},
		{"at most two retries", []reply{{429, "1"}, {429, "1"}, {429, "1"}}, []time.Duration{time.Second, time.Second}, &RateLimitError{429, time.Second}, 3},
		{"30s is still waited", []reply{{429, "30"}}, []time.Duration{30 * time.Second}, nil, 2},
		{"long wait returned", []reply{{429, "3600"}}, nil, &RateLimitError{429, time.Hour}, 1},
		{"423 long wait returned", []reply{{423, "31"}}, nil, &RateLimitError{423, 31 * time.Second}, 1},
		{"missing Retry-After waits 5s", []reply{{429, "-"}}, []time.Duration{5 * time.Second}, nil, 2},
		{"malformed Retry-After waits 5s", []reply{{429, "soon"}}, []time.Duration{5 * time.Second}, nil, 2},
		{"past HTTP-date retries at once", []reply{{429, "Wed, 21 Oct 2015 07:28:00 GMT"}}, []time.Duration{0}, nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n < len(tt.replies) {
					if ra := tt.replies[n].retryAfter; ra != "-" {
						w.Header().Set("Retry-After", ra)
					}
					w.WriteHeader(tt.replies[n].status)
					return
				}
				writeJSON(t, w, Message{ID: "m1"})
			})
			var sleeps []time.Duration
			msg, err := rec.client(&sleeps).GetMessage("m1")

			if tt.wantErr == nil {
				if err != nil || msg.ID != "m1" {
					t.Fatalf("GetMessage = %+v, %v", msg, err)
				}
			} else {
				var rl *RateLimitError
				if !errors.As(err, &rl) {
					t.Fatalf("err = %v, want *RateLimitError", err)
				}
				if *rl != *tt.wantErr {
					t.Errorf("err = %+v, want %+v", *rl, *tt.wantErr)
				}
			}
			if fmt.Sprint(sleeps) != fmt.Sprint(tt.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", sleeps, tt.wantSleeps)
			}
			if rec.count() != tt.wantCount {
				t.Errorf("requests = %d, want %d", rec.count(), tt.wantCount)
			}
			checkAuth(t, rec)
		})
	}
}

func TestRateLimitDuringPaging(t *testing.T) {
	rec := newRecorder(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 0:
			w.Header().Set("Link", `</v1/rooms?cursor=2>; rel="next"`)
			writeJSON(t, w, roomsPage(room("a", time.Now())))
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			if r.URL.Query().Get("cursor") != "2" {
				t.Errorf("retry went to %s", r.URL)
			}
			writeJSON(t, w, roomsPage(room("b", time.Now())))
		}
	})
	var sleeps []time.Duration
	rooms, err := rec.client(&sleeps).ListDirectRooms(time.Time{}, 0)
	if err != nil || ids(rooms) != "a,b" {
		t.Fatalf("rooms = %q, %v", ids(rooms), err)
	}
	if len(sleeps) != 1 || rec.count() != 3 {
		t.Errorf("sleeps = %v, requests = %d", sleeps, rec.count())
	}
	checkAuth(t, rec)
}

func TestPostTypedErrorsWithoutRetry(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		check     func(error) bool
		wantError string
	}{
		{"404 matches ErrNotFound", 404, func(err error) bool { return errors.Is(err, ErrNotFound) }, "webex API error (status 404)"},
		{"429 is a RateLimitError", 429, func(err error) bool {
			var rl *RateLimitError
			return errors.As(err, &rl) && rl.Status == 429 && rl.RetryAfter == time.Second
		}, "webex API rate limited (status 429), retry after 1s"},
		{"400 keeps the old message", 400, func(err error) bool { return !errors.Is(err, ErrNotFound) }, "webex API error (status 400)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tt.status)
			})
			var sleeps []time.Duration
			_, err := rec.client(&sleeps).SendMessage("r1", "", "", "", "hi", "")
			if err == nil || !tt.check(err) || err.Error() != tt.wantError {
				t.Errorf("err = %v, want %q", err, tt.wantError)
			}
			if rec.count() != 1 || len(sleeps) != 0 {
				t.Errorf("a POST must not be retried: requests %d, sleeps %v", rec.count(), sleeps)
			}
			if r := rec.req(0); r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			checkAuth(t, rec)
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", defaultRetryAfter},
		{"   ", defaultRetryAfter},
		{"0", 0},
		{"1", time.Second},
		{" 7 ", 7 * time.Second},
		{"3600", time.Hour},
		{"-1", defaultRetryAfter},
		{"1.5", defaultRetryAfter},
		{"soon", defaultRetryAfter},
		{"99999999999999999999", defaultRetryAfter}, // does not fit in int64
		{"9223372036854775807", maxRetryAfterSeconds * time.Second},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"Monday, 23-Nov-26 12:00:10 GMT", 10 * time.Second}, // RFC 850
		{"Mon Nov 23 12:00:20 2026", 20 * time.Second},       // asctime
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"none", nil, ""},
		{"quoted rel", []string{`<https://h/v1/rooms?c=2>; rel="next"`}, "https://h/v1/rooms?c=2"},
		{"bare rel", []string{`<https://h/v1/rooms?c=2>;rel=next`}, "https://h/v1/rooms?c=2"},
		{"case-insensitive", []string{`<https://h/a>; REL="Next"`}, "https://h/a"},
		{"second link", []string{`<https://h/p>; rel="prev", <https://h/n>; rel="next"`}, "https://h/n"},
		{"second header value", []string{`<https://h/p>; rel="prev"`, `<https://h/n>; rel="next"`}, "https://h/n"},
		{"quoted comma in params", []string{`<https://h/p>; title="a, <https://h/x>; rel=next"; rel="prev", <https://h/n>; rel="next"`}, "https://h/n"},
		{"rel list", []string{`<https://h/n>; rel="prev next"`}, "https://h/n"},
		{"not next", []string{`<https://h/p>; rel="prev"`, `<https://h/l>; rel="last"`}, ""},
		{"nextish is not next", []string{`<https://h/p>; rel="next-archive"`}, ""},
		{"rev is not rel", []string{`<https://h/p>; rev="next"`}, ""},
		{"malformed", []string{`https://h/n; rel="next"`}, ""},
		{"unterminated", []string{`<https://h/n; rel="next"`}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextLink(tt.values); got != tt.want {
				t.Errorf("nextLink(%q) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}
