package replydesk

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// BM25 parameters.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// denseMinWords is the word count at which a message is dense on length alone.
const denseMinWords = 6

// stopwords are common English words dropped before ranking.
var stopwords = map[string]bool{
	"a": true, "about": true, "after": true, "all": true, "also": true, "am": true,
	"an": true, "and": true, "any": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "can": true, "could": true,
	"did": true, "do": true, "does": true, "for": true, "from": true, "had": true,
	"has": true, "have": true, "he": true, "her": true, "him": true, "his": true,
	"how": true, "i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "just": true, "me": true, "my": true, "no": true,
	"not": true, "of": true, "on": true, "or": true, "our": true, "out": true,
	"she": true, "so": true, "than": true, "that": true, "the": true, "their": true,
	"them": true, "then": true, "there": true, "these": true, "they": true,
	"this": true, "to": true, "up": true, "us": true, "was": true, "we": true,
	"were": true, "what": true, "when": true, "where": true, "which": true,
	"who": true, "why": true, "will": true, "with": true, "would": true,
	"you": true, "your": true,
}

// word is one run of unicode letters or digits in a text.
type word struct {
	text string
	// sentenceStart is true when the word opens the text or follows a
	// sentence terminator (. ! ?) or a line break.
	sentenceStart bool
}

// splitWords returns the runs of unicode letters and digits in text.
func splitWords(text string) []word {
	var out []word
	start := -1
	boundary := true
	for i, r := range text {
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r)
		switch {
		case isWord && start < 0:
			start = i
		case !isWord && start >= 0:
			out = append(out, word{text: text[start:i], sentenceStart: boundary})
			boundary = false
			start = -1
		}
		if !isWord && (r == '.' || r == '!' || r == '?' || r == '\n') {
			boundary = true
		}
	}
	if start >= 0 {
		out = append(out, word{text: text[start:], sentenceStart: boundary})
	}
	return out
}

// Dense is the ADR 0001 density gate: at least 6 words, or naming something
// concrete. Only dense messages are worth retrieving.
//
// A word is a run of unicode letters or digits. Something concrete is a
// digit, an http:// or https:// URL, an email address, or a capitalized word
// of two or more runes that does not open a sentence (so "I" alone does not
// count).
func Dense(text string) bool {
	words := splitWords(text)
	if len(words) >= denseMinWords {
		return true
	}
	if strings.ContainsFunc(text, unicode.IsDigit) {
		return true
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return true
	}
	if hasEmail(text) {
		return true
	}
	for _, w := range words {
		if w.sentenceStart || utf8.RuneCountInString(w.text) < 2 {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(w.text); unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// hasEmail reports whether any whitespace-separated field looks like
// local@domain.tld.
func hasEmail(text string) bool {
	for _, f := range strings.Fields(text) {
		f = strings.Trim(f, "<>()[]{},;:\"'")
		at := strings.IndexByte(f, '@')
		if at <= 0 || at != strings.LastIndexByte(f, '@') {
			continue
		}
		domain := strings.TrimRight(f[at+1:], ".")
		dot := strings.LastIndexByte(domain, '.')
		if dot > 0 && dot < len(domain)-1 {
			return true
		}
	}
	return false
}

// tokenize lowercases text and returns its ranking terms: letter/digit runs
// of at least two runes that are not stopwords.
func tokenize(text string) []string {
	var out []string
	for _, w := range splitWords(text) {
		t := strings.ToLower(w.text)
		if utf8.RuneCountInString(t) < 2 || stopwords[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// messageText is the text a message is ranked and gated on.
func messageText(m *webex.Message) string {
	if m.Text != "" {
		return m.Text
	}
	return m.Markdown
}

// Rank returns up to k of mine (the user's own messages) most relevant to
// query, best first, using BM25 over dense messages with recency as the tie
// breaker. Messages with no term overlap are not returned.
func Rank(query string, mine []webex.Message, k int) []webex.Message {
	if k <= 0 {
		return nil
	}
	qTerms := uniq(tokenize(query))
	if len(qTerms) == 0 {
		return nil
	}

	type doc struct {
		msg   webex.Message
		tf    map[string]int
		len   int
		score float64
	}
	docs := make([]*doc, 0, len(mine))
	totalLen := 0
	for i := range mine {
		text := messageText(&mine[i])
		if !Dense(text) {
			continue
		}
		terms := tokenize(text)
		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		docs = append(docs, &doc{msg: mine[i], tf: tf, len: len(terms)})
		totalLen += len(terms)
	}
	if len(docs) == 0 || totalLen == 0 {
		return nil
	}
	n := float64(len(docs))
	avgLen := float64(totalLen) / n

	for _, q := range qTerms {
		df := 0
		for _, d := range docs {
			if d.tf[q] > 0 {
				df++
			}
		}
		if df == 0 {
			continue
		}
		// Lucene's non-negative IDF variant.
		idf := math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
		for _, d := range docs {
			f := float64(d.tf[q])
			if f == 0 {
				continue
			}
			norm := bm25K1 * (1 - bm25B + bm25B*float64(d.len)/avgLen)
			d.score += idf * f * (bm25K1 + 1) / (f + norm)
		}
	}

	hits := docs[:0]
	for _, d := range docs {
		if d.score > 0 {
			hits = append(hits, d)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return newer(hits[i].msg.Created, hits[j].msg.Created)
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	if len(hits) == 0 {
		return nil
	}
	out := make([]webex.Message, len(hits))
	for i, d := range hits {
		out[i] = d.msg
	}
	return out
}

// newer reports whether timestamp a is strictly newer than b. Unparseable
// timestamps sort as oldest.
func newer(a, b string) bool {
	ta, okA := parseTime(a)
	tb, okB := parseTime(b)
	switch {
	case okA && okB:
		return ta.After(tb)
	case okA:
		return true
	default:
		return false
	}
}

// uniq returns terms with duplicates removed, first occurrence kept.
func uniq(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := terms[:0]
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
