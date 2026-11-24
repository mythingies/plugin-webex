package replydesk

import (
	"reflect"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

func TestDense(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"one word", "ok", false},
		{"five lowercase words", "sure that works for me", false},
		{"six words", "sure that works for me too", true},
		{"six words with punctuation", "yes, yes; yes - yes! yes? yes.", true},
		{"unicode words", "café naïve résumé über straße jalapeño", true},
		{"digit", "at 3", true},
		{"digit inside word", "use v2", true},
		{"unicode digit", "room ٣", true},
		{"http url", "see http://x", true},
		{"https url uppercase scheme", "see HTTPS://x", true},
		{"email", "ask bob@example.com", true},
		{"email in angle brackets", "ask <bob@example.com>", true},
		{"at sign without domain dot", "ping @bob", false},
		{"at sign with bare host", "mail bob@localhost", false},
		{"capitalized word mid-sentence", "ask Bob", true},
		{"acronym mid-sentence", "check the PR", true},
		{"capitalized first word only", "Thanks", false},
		{"capitalized first words of sentences", "Thanks. Sure! Okay?", false},
		{"capital after newline", "thanks\nSure", false},
		{"capital after comma counts", "thanks, Sure", true},
		{"pronoun I is not concrete", "ok I see", false},
		{"leading whitespace first word", "  Hello there", false},
		{"unicode capital mid-sentence", "ask Émile", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Dense(tt.text); got != tt.want {
				t.Errorf("Dense(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("The Deploy-Script, is it in Q4? a x é ok ÉTÉ")
	want := []string{"deploy", "script", "q4", "ok", "été"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
}

func msg(id, text string, ago time.Duration) webex.Message {
	return webex.Message{ID: id, PersonID: meID, Text: text, Created: ts(ago)}
}

func TestRank(t *testing.T) {
	deploy := msg("deploy", "the deploy script lives in the infra repo under tools", 5*time.Hour)
	deployTwice := msg("deploy2", "deploy deploy: run the deploy script before the release window", 6*time.Hour)
	budget := msg("budget", "the Q4 budget was approved by finance last week", 3*time.Hour)
	shortNonDense := msg("short", "deploy later", time.Hour) // overlaps, but fails the density gate
	unrelated := msg("lunch", "let us grab lunch near the office tomorrow maybe", 2*time.Hour)
	markdownOnly := webex.Message{ID: "md", PersonID: meID, Markdown: "the **budget** spreadsheet is on the shared drive now", Created: ts(4 * time.Hour)}

	corpus := []webex.Message{deploy, deployTwice, budget, shortNonDense, unrelated, markdownOnly}

	tests := []struct {
		name  string
		query string
		mine  []webex.Message
		k     int
		want  []string
	}{
		{"k zero", "deploy", corpus, 0, nil},
		{"k negative", "deploy", corpus, -1, nil},
		{"empty corpus", "deploy", nil, 3, nil},
		{"empty query", "", corpus, 3, nil},
		{"stopword-only query", "what is the", corpus, 3, nil},
		{"no overlap", "kubernetes helm", corpus, 3, nil},
		{"term frequency ranks higher", "where is the deploy script?", corpus, 5, []string{"deploy2", "deploy"}},
		{"k limits results", "where is the deploy script?", corpus, 1, []string{"deploy2"}},
		{"case insensitive, markdown fallback, shorter doc first", "BUDGET", corpus, 5, []string{"md", "budget"}},
		{"non-dense filtered", "later", corpus, 5, nil},
		{"rare term outranks common", "budget finance", corpus, 5, []string{"budget", "md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Rank(tt.query, tt.mine, tt.k)
			var gotIDs []string
			if got != nil {
				gotIDs = ids(got)
			}
			if !reflect.DeepEqual(gotIDs, tt.want) {
				t.Errorf("Rank = %v, want %v", gotIDs, tt.want)
			}
		})
	}
}

func TestRankTiesNewerFirst(t *testing.T) {
	text := "the release checklist is pinned in the team space"
	older := msg("older", text, 48*time.Hour)
	newest := msg("newest", text, time.Hour)
	middle := msg("middle", text, 24*time.Hour)
	undated := webex.Message{ID: "undated", PersonID: meID, Text: text, Created: "not a date"}
	filler := msg("filler", "completely different words about gardening and weather patterns", time.Minute)

	got := Rank("release checklist", []webex.Message{older, undated, newest, filler, middle}, 10)
	want := []string{"newest", "middle", "older", "undated"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("Rank = %v, want %v", gotIDs, want)
	}
}

func TestRankLengthNormalization(t *testing.T) {
	short := msg("short", "the invoice number is pending approval", 2*time.Hour)
	long := msg("long", "the invoice number is pending approval and also here are many unrelated words about travel plans hotels flights and meetings", time.Hour)
	got := Rank("invoice", []webex.Message{long, short}, 2)
	want := []string{"short", "long"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("Rank = %v, want %v", gotIDs, want)
	}
}

func TestRankDoesNotMutateInput(t *testing.T) {
	in := []webex.Message{
		msg("a", "alpha beta gamma delta epsilon zeta", 3*time.Hour),
		msg("b", "alpha alpha alpha beta gamma delta", 2*time.Hour),
	}
	want := ids(in)
	_ = Rank("alpha", in, 2)
	if got := ids(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("input reordered: %v, want %v", got, want)
	}
}
