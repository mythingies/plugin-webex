package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/replydesk"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func TestParseLookback(t *testing.T) {
	ok := map[string]time.Duration{
		"7d":  7 * 24 * time.Hour,
		"1h":  time.Hour,
		"36h": 36 * time.Hour,
		"90d": 90 * 24 * time.Hour,
		" 2d": 48 * time.Hour,
	}
	for in, want := range ok {
		got, err := parseLookback(in)
		if err != nil || got != want {
			t.Errorf("parseLookback(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "30m", "91d", "xd", "-1d", "7days"} {
		if _, err := parseLookback(in); err == nil {
			t.Errorf("parseLookback(%q) succeeded, want error", in)
		}
	}
}

func TestParseReviewArgs(t *testing.T) {
	opts, err := parseReviewArgs([]string{"--dry-run", "--lookback=3d", "--drafter=off", "--no-browser"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.dryRun || !opts.noBrowser || opts.lookback != 72*time.Hour || opts.drafter != replydesk.ModeOff {
		t.Errorf("unexpected options: %+v", opts)
	}

	defaults, err := parseReviewArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.lookback != replydesk.DefaultLookback || defaults.drafter != replydesk.ModeAuto {
		t.Errorf("unexpected defaults: %+v", defaults)
	}

	for _, bad := range [][]string{{"--drafter=yolo"}, {"--lookback=1y"}, {"extra"}, {"--unknown"}} {
		if _, err := parseReviewArgs(bad, io.Discard); err == nil {
			t.Errorf("parseReviewArgs(%v) succeeded, want error", bad)
		}
	}
}

func TestWriteDryRun(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := writeDryRun(&buf, nil, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No unanswered") {
		t.Errorf("empty output: %q", buf.String())
	}

	buf.Reset()
	err := writeDryRun(&buf, []replydesk.Pending{{
		RoomTitle:   "Alice",
		HeadCreated: now.Add(-90 * time.Minute),
		Reason:      "2 messages since your last reply on 2026-11-22 09:00",
		Flags:       []string{replydesk.FlagAttachment},
		Burst: []webex.Message{
			{Text: "hey,\n  quick one"},
			{Text: ""},
		},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"1 direct message", "Alice (1h30m ago)", "2 messages since", "flags: attachment", "> hey, quick one", "> [no text]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPreview(t *testing.T) {
	if got := preview(strings.Repeat("é", 200), 10); len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("preview truncation wrong: %q", got)
	}
}

// errWriter fails every write.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteDryRun_ReturnsWriteError(t *testing.T) {
	now := time.Now()
	if err := writeDryRun(errWriter{}, nil, now); err == nil {
		t.Error("writeDryRun ignored a write error on the empty list")
	}
	p := []replydesk.Pending{{RoomTitle: "Alice", HeadCreated: now, Burst: []webex.Message{{Text: "hi"}}}}
	if err := writeDryRun(errWriter{}, p, now); err == nil {
		t.Error("writeDryRun ignored a write error")
	}
}

func TestReviewDryRun_PropagatesWriteError(t *testing.T) {
	if err := reviewDryRun(context.Background(), &fakeAPI{}, time.Hour, errWriter{}, io.Discard); err == nil {
		t.Error("reviewDryRun did not propagate the stdout write error")
	}
}

// Sender-controlled text must not carry terminal escape sequences, line
// breaks, or bidi overrides to the terminal.
func TestWriteDryRun_StripsControlCharacters(t *testing.T) {
	esc := string(rune(0x1b))
	bel := string(rune(0x07))
	csi := string(rune(0x9b)) // C1 control sequence introducer
	rlo := string(rune(0x202e))
	lri := string(rune(0x2066))
	ls := string(rune(0x2028))
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := writeDryRun(&buf, []replydesk.Pending{{
		RoomTitle:   "Mallory" + esc + "]0;pwned" + bel + rlo + "txt.exe",
		HeadCreated: now,
		Reason:      "1 message" + esc + "[2J" + ls + "fake line",
		Burst: []webex.Message{
			{Text: "click" + esc + "[31m here" + csi + "1m" + lri + "x"},
		},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{esc, bel, csi, rlo, lri, ls} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains control character %U:\n%q", []rune(bad)[0], out)
		}
	}
	for _, want := range []string{"Mallory ]0;pwned txt.exe (<1m ago)", "1 message [2J fake line", "> click [31m here 1m x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%q", want, out)
		}
	}
}

func TestTerminalSafe(t *testing.T) {
	in := "a" + string(rune(0x7f)) + "b" + string(rune(0x200f)) + "c" + string(rune(0x2029)) + "d" + string(rune(13)) + string(rune(10)) + "e" + string(rune(9)) + "f"
	if got := terminalSafe(in); got != "a b c d e f" {
		t.Errorf("terminalSafe = %q", got)
	}
	if got := terminalSafe("héllo wörld ok"); got != "héllo wörld ok" {
		t.Errorf("terminalSafe changed printable text: %q", got)
	}
}

// fakeAPI is a replydesk.API for dry-run tests.
type fakeAPI struct {
	rooms   []webex.Space
	listErr error
	msgErr  map[string]error // per room ID
}

func (f *fakeAPI) GetMe() (*webex.Person, error) { return &webex.Person{ID: "me"}, nil }

func (f *fakeAPI) ListDirectRooms(time.Time, int) ([]webex.Space, error) {
	return f.rooms, f.listErr
}

func (f *fakeAPI) ListMessages(opts webex.ListMessagesOptions) ([]webex.Message, error) {
	if err := f.msgErr[opts.RoomID]; err != nil {
		return nil, err
	}
	return nil, nil
}

func (f *fakeAPI) GetMessage(string) (*webex.Message, error) { return nil, webex.ErrNotFound }

func (f *fakeAPI) SendMessage(string, string, string, string, string, string) (*webex.Message, error) {
	return nil, errors.New("dry run must not send")
}

func TestReviewDryRun_FailedRoomIsAWarning(t *testing.T) {
	api := &fakeAPI{
		rooms:  []webex.Space{{ID: "r1", Type: "direct"}, {ID: "r2", Type: "direct"}},
		msgErr: map[string]error{"r1": errors.New("HTTP 500")},
	}
	var out, errOut bytes.Buffer
	if err := reviewDryRun(context.Background(), api, 24*time.Hour, &out, &errOut); err != nil {
		t.Fatalf("one failed room with nothing pending must not be fatal: %v", err)
	}
	if !strings.Contains(out.String(), "No unanswered") {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "warning: some rooms could not be scanned") || !strings.Contains(errOut.String(), "HTTP 500") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestReviewDryRun_RateLimitIsFatal(t *testing.T) {
	api := &fakeAPI{
		rooms:  []webex.Space{{ID: "r1", Type: "direct"}},
		msgErr: map[string]error{"r1": &webex.RateLimitError{Status: 429, RetryAfter: time.Minute}},
	}
	var out bytes.Buffer
	err := reviewDryRun(context.Background(), api, 24*time.Hour, &out, io.Discard)
	var rl *webex.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want a RateLimitError", err)
	}
	if strings.Contains(out.String(), "No unanswered") {
		t.Errorf("a rate-limited scan must not print an all-clear: %q", out.String())
	}
}

func TestReviewDryRun_ListingFailureIsFatal(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("HTTP 401")}
	var out bytes.Buffer
	if err := reviewDryRun(context.Background(), api, 24*time.Hour, &out, io.Discard); err == nil {
		t.Fatal("a failed room listing must be fatal")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed when nothing could be listed: %q", out.String())
	}
}

func TestDryRunFatal(t *testing.T) {
	bg := context.Background()
	roomErrs := errors.Join(errors.New("room a: boom"), errors.New("room b: boom"))
	if dryRunFatal(bg, roomErrs, 0) {
		t.Error("per-room errors alone are not fatal")
	}
	if !dryRunFatal(bg, errors.Join(errors.New("listing direct rooms: x"), errors.New("room a: y")), 0) {
		t.Error("a listing error with nothing pending is fatal")
	}
	if dryRunFatal(bg, errors.Join(errors.New("listing direct rooms: x")), 2) {
		t.Error("a partial listing error with results is a warning")
	}
	if !dryRunFatal(bg, fmt.Errorf("wrapped: %w", &webex.RateLimitError{}), 3) {
		t.Error("a rate limit is fatal even with results")
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	if !dryRunFatal(cancelled, roomErrs, 1) {
		t.Error("an interrupted scan is fatal")
	}
	if dryRunFatal(bg, nil, 0) {
		t.Error("nil is not fatal")
	}
}

func TestServeThenClose_ClosesAfterServe(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // Ctrl-C: the serve context is already done at shutdown
	serveErr := errors.New("listen failed")
	closeErr := errors.New("close failed")
	var order []string
	err := serveThenClose(parent,
		func(context.Context) error { order = append(order, "serve"); return serveErr },
		func(ctx context.Context) error {
			order = append(order, "close")
			if ctx.Err() != nil {
				t.Error("close got an already-cancelled context")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("close context has no deadline")
			}
			return closeErr
		}, time.Second)
	if strings.Join(order, ",") != "serve,close" {
		t.Errorf("order = %v", order)
	}
	if !errors.Is(err, serveErr) || !errors.Is(err, closeErr) {
		t.Errorf("err = %v, want both errors", err)
	}

	order = nil
	if err := serveThenClose(context.Background(),
		func(context.Context) error { order = append(order, "serve"); return nil },
		func(context.Context) error { order = append(order, "close"); return nil },
		time.Second); err != nil {
		t.Errorf("err = %v", err)
	}
	if len(order) != 2 {
		t.Errorf("close not called after a clean serve: %v", order)
	}
}

func TestReviewFailure_AlreadyRunning(t *testing.T) {
	err := fmt.Errorf("opening Reply Desk state: %w", replydesk.ErrAlreadyRunning)
	if got := reviewFailure(err); got != alreadyOpenMessage {
		t.Errorf("reviewFailure = %q", got)
	}
	if !strings.HasPrefix(alreadyOpenMessage, "Reply Desk is already open in another window") {
		t.Errorf("message = %q", alreadyOpenMessage)
	}
	if got := reviewFailure(errors.New("boom")); got != "review: boom" {
		t.Errorf("reviewFailure = %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Second:                "<1m",
		45 * time.Minute:                "45m",
		3*time.Hour + 31*time.Minute:    "3h31m",
		2 * time.Hour:                   "2h",
		53*time.Hour + 10*time.Minute:   "2d5h",
		7 * 24 * time.Hour:              "7d",
		7*24*time.Hour + 20*time.Second: "7d",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
