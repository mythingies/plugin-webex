package replydesk

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

const (
	meID    = "me-id"
	otherID = "other-id"
)

var (
	testNow  = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	testRoom = webex.Space{ID: "room-1", Title: "Alice Example", Type: "direct"}
)

// ts returns an RFC 3339 timestamp d before testNow.
func ts(d time.Duration) string { return testNow.Add(-d).Format(time.RFC3339Nano) }

func theirs(id string, ago time.Duration) webex.Message {
	return webex.Message{ID: id, PersonID: otherID, PersonEmail: "alice@example.com", Created: ts(ago), Text: "text " + id}
}

func mine(id string, ago time.Duration) webex.Message {
	return webex.Message{ID: id, PersonID: meID, PersonEmail: "me@example.com", Created: ts(ago), Text: "text " + id}
}

func ids(msgs []webex.Message) []string {
	out := make([]string, len(msgs))
	for i := range msgs {
		out[i] = msgs[i].ID
	}
	return out
}

func TestDetectNil(t *testing.T) {
	bot := theirs("b", time.Minute)
	bot.PersonEmail = "helper@webex.bot"
	botOlder := theirs("b2", 2*time.Minute)
	botOlder.PersonEmail = "Helper@SparkBot.io"
	badDate := theirs("x", time.Minute)
	badDate.Created = "yesterday"
	emptyDate := theirs("x", time.Minute)
	emptyDate.Created = ""

	tests := []struct {
		name    string
		room    webex.Space
		history []webex.Message
	}{
		{"empty history", testRoom, nil},
		{"answered: newest is mine", testRoom, []webex.Message{mine("m1", time.Minute), theirs("t1", time.Hour)}},
		{"only my messages", testRoom, []webex.Message{mine("m1", time.Minute), mine("m2", time.Hour)}},
		{"group room", webex.Space{ID: "g", Type: "group"}, []webex.Message{theirs("t1", time.Minute)}},
		{"bot sender", testRoom, []webex.Message{bot, mine("m1", time.Hour)}},
		{"bot sender later in burst, mixed case", testRoom, []webex.Message{theirs("t1", time.Minute), botOlder}},
		{"head older than lookback", testRoom, []webex.Message{theirs("t1", DefaultLookback+time.Second), mine("m1", 8*24*time.Hour)}},
		{"unparseable head", testRoom, []webex.Message{badDate, mine("m1", time.Hour)}},
		{"empty head created", testRoom, []webex.Message{emptyDate}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(meID, tt.room, tt.history, testNow, DefaultLookback); got != nil {
				t.Fatalf("Detect = %+v, want nil", got)
			}
		})
	}
}

func TestDetectRoomType(t *testing.T) {
	history := []webex.Message{theirs("t1", time.Minute)}
	for _, typ := range []string{"", "direct"} {
		room := testRoom
		room.Type = typ
		if Detect(meID, room, history, testNow, DefaultLookback) == nil {
			t.Errorf("room type %q: Detect = nil, want pending", typ)
		}
	}
}

func TestDetectLookbackBoundary(t *testing.T) {
	history := []webex.Message{theirs("t1", DefaultLookback)}
	if Detect(meID, testRoom, history, testNow, DefaultLookback) == nil {
		t.Fatal("head exactly at lookback: Detect = nil, want pending")
	}
	if Detect(meID, testRoom, history, testNow, DefaultLookback-time.Nanosecond) != nil {
		t.Fatal("head just past lookback: Detect != nil, want nil")
	}
}

func TestDetectRFC3339WithoutFraction(t *testing.T) {
	m := theirs("t1", time.Minute)
	m.Created = "2026-11-23T11:59:00Z"
	p := Detect(meID, testRoom, []webex.Message{m}, testNow, DefaultLookback)
	if p == nil {
		t.Fatal("Detect = nil")
	}
	if want := time.Date(2026, 11, 23, 11, 59, 0, 0, time.UTC); !p.HeadCreated.Equal(want) {
		t.Errorf("HeadCreated = %v, want %v", p.HeadCreated, want)
	}
}

func TestDetectBurstAndContext(t *testing.T) {
	// Builds a history, newest first, of nTheirs messages from the other
	// person, then my reply, then nOlder alternating older messages.
	build := func(nTheirs, nOlder int, withMine bool) []webex.Message {
		var h []webex.Message
		ago := time.Minute
		for i := range nTheirs {
			h = append(h, theirs(fmt.Sprintf("t%d", i), ago))
			ago += time.Minute
		}
		if withMine {
			h = append(h, mine("mine", ago))
			ago += time.Minute
		}
		for i := range nOlder {
			if i%2 == 0 {
				h = append(h, theirs(fmt.Sprintf("o%d", i), ago))
			} else {
				h = append(h, mine(fmt.Sprintf("o%d", i), ago))
			}
			ago += time.Minute
		}
		return h
	}
	seq := func(prefix string, from, to int) []string { // from > to allowed: descending
		var out []string
		step := 1
		if from > to {
			step = -1
		}
		for i := from; ; i += step {
			out = append(out, fmt.Sprintf("%s%d", prefix, i))
			if i == to {
				break
			}
		}
		return out
	}

	tests := []struct {
		name        string
		history     []webex.Message
		wantBurst   []string
		wantContext []string
		wantFlags   []string
	}{
		{
			name:        "single message, my reply, no older",
			history:     build(1, 0, true),
			wantBurst:   []string{"t0"},
			wantContext: []string{"mine"},
			wantFlags:   []string{},
		},
		{
			name:        "burst stops at my message",
			history:     build(3, 2, true),
			wantBurst:   []string{"t2", "t1", "t0"},
			wantContext: []string{"o1", "o0", "mine"},
			wantFlags:   []string{},
		},
		{
			name:        "context capped at ContextMessages",
			history:     build(2, 20, true),
			wantBurst:   []string{"t1", "t0"},
			wantContext: append(seq("o", ContextMessages-2, 0), "mine"),
			wantFlags:   []string{},
		},
		{
			name:        "no history from me",
			history:     build(4, 0, false),
			wantBurst:   []string{"t3", "t2", "t1", "t0"},
			wantContext: []string{},
			wantFlags:   []string{FlagNoHistory},
		},
		{
			name:        "burst exactly MaxBurst is not truncated",
			history:     build(MaxBurst, 0, true),
			wantBurst:   seq("t", MaxBurst-1, 0),
			wantContext: []string{"mine"},
			wantFlags:   []string{},
		},
		{
			name:        "long burst truncated to newest MaxBurst",
			history:     build(MaxBurst+5, 3, true),
			wantBurst:   seq("t", MaxBurst-1, 0),
			wantContext: []string{"o2", "o1", "o0", "mine"},
			wantFlags:   []string{FlagLongBurst},
		},
		{
			name:        "long burst with no history",
			history:     build(MaxBurst+1, 0, false),
			wantBurst:   seq("t", MaxBurst-1, 0),
			wantContext: []string{},
			wantFlags:   []string{FlagLongBurst, FlagNoHistory},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Detect(meID, testRoom, tt.history, testNow, DefaultLookback)
			if p == nil {
				t.Fatal("Detect = nil")
			}
			if got := ids(p.Burst); !reflect.DeepEqual(got, tt.wantBurst) {
				t.Errorf("Burst = %v, want %v", got, tt.wantBurst)
			}
			if got := ids(p.Context); !reflect.DeepEqual(got, tt.wantContext) {
				t.Errorf("Context = %v, want %v", got, tt.wantContext)
			}
			if !reflect.DeepEqual(p.Flags, tt.wantFlags) {
				t.Errorf("Flags = %v, want %v", p.Flags, tt.wantFlags)
			}
			if p.HeadID != "t0" {
				t.Errorf("HeadID = %q, want t0", p.HeadID)
			}
			if !p.HeadCreated.Equal(testNow.Add(-time.Minute)) {
				t.Errorf("HeadCreated = %v", p.HeadCreated)
			}
			oldest, _ := time.Parse(time.RFC3339Nano, p.Burst[0].Created)
			if !p.Since.Equal(oldest) {
				t.Errorf("Since = %v, want %v", p.Since, oldest)
			}
			if p.RoomID != testRoom.ID || p.RoomTitle != testRoom.Title {
				t.Errorf("room = %q/%q", p.RoomID, p.RoomTitle)
			}
		})
	}
}

func TestDetectDoesNotMutateInput(t *testing.T) {
	history := []webex.Message{theirs("t0", time.Minute), theirs("t1", 2*time.Minute), mine("m", 3*time.Minute)}
	want := ids(history)
	_ = Detect(meID, testRoom, history, testNow, DefaultLookback)
	if got := ids(history); !reflect.DeepEqual(got, want) {
		t.Fatalf("input reordered: %v, want %v", got, want)
	}
}

func TestDetectFlags(t *testing.T) {
	with := func(f func(*webex.Message)) webex.Message {
		m := theirs("t1", 2*time.Minute)
		f(&m)
		return m
	}
	file := with(func(m *webex.Message) { m.Files = []string{"https://files/1"} })
	card := with(func(m *webex.Message) { m.Attachments = []json.RawMessage{json.RawMessage(`{}`)} })
	voice := with(func(m *webex.Message) { m.IsVoiceClip = true })
	edited := with(func(m *webex.Message) { m.Updated = ts(time.Minute) })
	all := with(func(m *webex.Message) {
		m.Files = []string{"f"}
		m.Attachments = []json.RawMessage{json.RawMessage(`{}`)}
		m.IsVoiceClip = true
		m.Updated = ts(time.Minute)
	})
	oldFile := mine("m", 3*time.Hour)
	oldFile.Files = []string{"f"} // my own attachment is not a burst flag

	tests := []struct {
		name  string
		burst []webex.Message
		want  []string
	}{
		{"plain", []webex.Message{theirs("t0", time.Minute)}, []string{}},
		{"files", []webex.Message{theirs("t0", time.Minute), file}, []string{FlagAttachment}},
		{"card", []webex.Message{card}, []string{FlagCard}},
		{"voice clip", []webex.Message{voice}, []string{FlagVoiceClip}},
		{"edited", []webex.Message{edited}, []string{FlagEdited}},
		{"all, declaration order", []webex.Message{all}, []string{FlagAttachment, FlagCard, FlagVoiceClip, FlagEdited}},
		{"deduplicated", []webex.Message{file, file, voice, file}, []string{FlagAttachment, FlagVoiceClip}},
		{"reverse order still declaration order", []webex.Message{edited, voice, card, file}, []string{FlagAttachment, FlagCard, FlagVoiceClip, FlagEdited}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := append(append([]webex.Message{}, tt.burst...), oldFile)
			// Context-only flagged messages must not count.
			context := theirs("c", 4*time.Hour)
			context.IsVoiceClip = true
			history = append(history, context)
			p := Detect(meID, testRoom, history, testNow, DefaultLookback)
			if p == nil {
				t.Fatal("Detect = nil")
			}
			if !reflect.DeepEqual(p.Flags, tt.want) {
				t.Errorf("Flags = %v, want %v", p.Flags, tt.want)
			}
		})
	}
}

func TestDetectHeadFields(t *testing.T) {
	head := theirs("head", time.Minute)
	head.ParentID = "parent-1"
	head.Updated = ts(30 * time.Second)
	older := theirs("older", time.Hour)
	older.ParentID = "parent-other"

	tests := []struct {
		name       string
		history    []webex.Message
		wantParent string
		wantUpd    string
	}{
		{"thread reply head", []webex.Message{head, older, mine("m", 2*time.Hour)}, "parent-1", head.Updated},
		{"top-level head after thread reply", []webex.Message{theirs("head", time.Minute), older}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Detect(meID, testRoom, tt.history, testNow, DefaultLookback)
			if p == nil {
				t.Fatal("Detect = nil")
			}
			if p.ParentID != tt.wantParent {
				t.Errorf("ParentID = %q, want %q", p.ParentID, tt.wantParent)
			}
			if p.HeadUpdated != tt.wantUpd {
				t.Errorf("HeadUpdated = %q, want %q", p.HeadUpdated, tt.wantUpd)
			}
			if p.HeadID != "head" {
				t.Errorf("HeadID = %q", p.HeadID)
			}
		})
	}
}

func TestDetectSinceUnparseable(t *testing.T) {
	old := theirs("t1", time.Hour)
	old.Created = "garbage"
	p := Detect(meID, testRoom, []webex.Message{theirs("t0", time.Minute), old}, testNow, DefaultLookback)
	if p == nil {
		t.Fatal("Detect = nil")
	}
	if !p.Since.IsZero() {
		t.Errorf("Since = %v, want zero", p.Since)
	}
}

func TestDetectReason(t *testing.T) {
	est := time.FixedZone("EST", -5*3600)
	myReply := mine("m", 0)
	myReply.Created = "2026-11-20T19:02:00.123Z"
	badReply := mine("m", 0)
	badReply.Created = "nope"

	tests := []struct {
		name    string
		history []webex.Message
		now     time.Time
		want    string
	}{
		{
			name:    "singular with reply",
			history: []webex.Message{theirs("t0", time.Minute), myReply},
			now:     testNow,
			want:    "1 message since your last reply on 2026-11-20 19:02",
		},
		{
			name:    "plural with reply",
			history: []webex.Message{theirs("t0", time.Minute), theirs("t1", 2*time.Minute), theirs("t2", 3*time.Minute), myReply},
			now:     testNow,
			want:    "3 messages since your last reply on 2026-11-20 19:02",
		},
		{
			name:    "reply time in now's location",
			history: []webex.Message{theirs("t0", time.Minute), myReply},
			now:     testNow.In(est),
			want:    "1 message since your last reply on 2026-11-20 14:02",
		},
		{
			name:    "singular without reply",
			history: []webex.Message{theirs("t0", time.Minute)},
			now:     testNow,
			want:    "1 message; no reply from you in the fetched history",
		},
		{
			name:    "plural without reply",
			history: []webex.Message{theirs("t0", time.Minute), theirs("t1", 2*time.Minute)},
			now:     testNow,
			want:    "2 messages; no reply from you in the fetched history",
		},
		{
			name:    "unparseable reply time",
			history: []webex.Message{theirs("t0", time.Minute), badReply},
			now:     testNow,
			want:    "1 message since your last reply",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Detect(meID, testRoom, tt.history, tt.now, DefaultLookback)
			if p == nil {
				t.Fatal("Detect = nil")
			}
			if p.Reason != tt.want {
				t.Errorf("Reason = %q, want %q", p.Reason, tt.want)
			}
		})
	}
}

func TestDetectReasonCountsWholeLongBurst(t *testing.T) {
	var h []webex.Message
	for i := range MaxBurst + 2 {
		h = append(h, theirs(fmt.Sprintf("t%d", i), time.Duration(i+1)*time.Minute))
	}
	p := Detect(meID, testRoom, h, testNow, DefaultLookback)
	if p == nil {
		t.Fatal("Detect = nil")
	}
	want := fmt.Sprintf("%d messages; no reply from you in the fetched history", MaxBurst+2)
	if p.Reason != want {
		t.Errorf("Reason = %q, want %q", p.Reason, want)
	}
	if len(p.Burst) != MaxBurst {
		t.Errorf("len(Burst) = %d, want %d", len(p.Burst), MaxBurst)
	}
}

func TestIsBot(t *testing.T) {
	tests := []struct {
		email string
		want  bool
	}{
		{"helper@webex.bot", true},
		{"Helper@WEBEX.BOT", true},
		{"helper@sparkbot.io", true},
		{"helper@SparkBot.IO", true},
		{"helper@eu.webex.bot", true},
		{"helper@a.b.sparkbot.io", true},
		{" helper@webex.bot ", true},
		{"helper@webex.bot.", true},
		{"alice@example.com", false},
		{"alice@notwebex.bot", false},
		{"alice@sparkbot.io.example.com", false},
		{"alice@webex.com", false},
		{"webex.bot", false},
		{"", false},
		{"@", false},
		{"bot@", false},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			if got := IsBot(tt.email); got != tt.want {
				t.Errorf("IsBot(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}
