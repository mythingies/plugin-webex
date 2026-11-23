package main

import (
	"bytes"
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
	writeDryRun(&buf, nil, now)
	if !strings.Contains(buf.String(), "No unanswered") {
		t.Errorf("empty output: %q", buf.String())
	}

	buf.Reset()
	writeDryRun(&buf, []replydesk.Pending{{
		RoomTitle:   "Alice",
		HeadCreated: now.Add(-90 * time.Minute),
		Reason:      "2 messages since your last reply on 2026-11-22 09:00",
		Flags:       []string{replydesk.FlagAttachment},
		Burst: []webex.Message{
			{Text: "hey,\n  quick one"},
			{Text: ""},
		},
	}}, now)
	out := buf.String()
	for _, want := range []string{"1 direct message", "Alice (1h30m0s ago)", "2 messages since", "flags: attachment", "> hey, quick one", "> [no text]"} {
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
