// Package safetext holds the text-safety rules shared by the MCP tools and
// Reply Desk: wrapping inbound Webex text for an LLM, and checking outbound
// text before it is sent.
package safetext

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// MaxMessageLen is the Webex message limit, checked on the raw input in bytes
// (len), exactly as the MCP tools always have.
const MaxMessageLen = 7439

// ErrEmpty is returned by Prepare when a required message is empty.
var ErrEmpty = errors.New("text or markdown is required")

// TooLongError is returned by Prepare when a field exceeds MaxMessageLen.
type TooLongError struct{ Len int }

func (e *TooLongError) Error() string {
	return fmt.Sprintf("message too long (%d chars, max %d)", e.Len, MaxMessageLen)
}

// wrapperTagRe matches the "<" that begins an <external-message> or
// </external-message> tag, case-insensitively and with optional whitespace
// around the slash. Only the "<" is replaced; the rest is kept via $1.
var wrapperTagRe = regexp.MustCompile(`(?i)<(\s*/?\s*external-message)`)

// Sandbox wraps Webex-sourced text so the LLM treats it as data, not
// instructions. Every opening or closing wrapper tag inside text has its "<"
// replaced with "&lt;", so the text can neither close the wrapper early nor
// open a nested one. Nothing else is escaped: whitespace, newlines, and text
// the listener already HTML-escaped ("&lt;/external-message&gt;") pass
// through unchanged.
//
// A partial wrapper tag at the very end of text, such as the
// "</external-messa" a truncation can leave behind, is neutralized the same
// way, so it cannot run into the closing wrapper and read as a tag.
func Sandbox(text string) string {
	return "<external-message>" + escapeWrapperTags(text) + "</external-message>"
}

// SandboxPreview is Sandbox for a preview of text cut to at most maxRunes
// runes. The cut lands on a rune boundary, "..." inside the wrapper marks
// that text was cut, and a wrapper tag split by the cut is neutralized. A
// maxRunes below 1 keeps the whole text.
func SandboxPreview(text string, maxRunes int) string {
	cut, ok := CutRunes(text, maxRunes)
	if !ok {
		return Sandbox(text)
	}
	return "<external-message>" + escapeWrapperTags(cut) + "...</external-message>"
}

// CutRunes returns the first max runes of s and true, or s unchanged and
// false when s has no more than max runes or max is below 1. It never splits
// a UTF-8 sequence.
func CutRunes(s string, max int) (string, bool) {
	if max < 1 {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// wrapperName is the tag name Sandbox wraps text in.
const wrapperName = "external-message"

// tagSpace is the whitespace wrapperTagRe's \s matches.
const tagSpace = " \t\n\f\r"

// escapeWrapperTags replaces the "<" of every whole wrapper tag in s, and of
// a partial one at the very end of s, with "&lt;".
func escapeWrapperTags(s string) string {
	s = wrapperTagRe.ReplaceAllString(s, "&lt;$1")
	i := strings.LastIndexByte(s, '<')
	if i < 0 || !isPartialWrapperTag(s[i+1:]) {
		return s
	}
	return s[:i] + "&lt;" + s[i+1:]
}

// isPartialWrapperTag reports whether rest, the text after a "<", is
// optional whitespace, an optional "/", optional whitespace, and then a
// non-empty case-insensitive prefix of the wrapper name, with nothing after
// it.
func isPartialWrapperTag(rest string) bool {
	rest = strings.TrimLeft(rest, tagSpace)
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimLeft(rest, tagSpace)
	return rest != "" && len(rest) <= len(wrapperName) && strings.EqualFold(rest, wrapperName[:len(rest)])
}

// MaxLabelRunes caps the length of a Label result, ellipsis included.
const MaxLabelRunes = 120

// labelReplacer swaps the characters that could start a tag (the Sandbox
// wrapper included) or a code span for look-alikes that cannot.
var labelReplacer = strings.NewReplacer("<", "‹", ">", "›", "`", "'")

// Label cleans a short Webex-controlled label, such as a space title or a
// display name, for one line of tool output. Control characters, newlines
// and other whitespace become single spaces and the ends are trimmed;
// bidirectional embedding, override and isolate characters are dropped; "<"
// and ">" become "‹" and "›", so no tag, wrapper or otherwise,
// survives; backticks become "'"; and the result is capped at MaxLabelRunes
// runes, ending in "…" when cut. Message bodies take Sandbox, not Label.
func Label(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case isBidiControl(r):
			continue
		case unicode.IsControl(r) || unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	out := labelReplacer.Replace(b.String())
	if _, cut := CutRunes(out, MaxLabelRunes); cut {
		head, _ := CutRunes(out, MaxLabelRunes-1)
		out = strings.TrimRight(head, " ") + "…"
	}
	return out
}

// isBidiControl reports whether r is a Unicode bidirectional embedding,
// override or isolate control, which can make a label render reordered.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// allowedOutboundProtocols lists URL schemes safe for outbound messages.
var allowedOutboundProtocols = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
}

// outboundSchemeRe matches a URI scheme followed by a non-empty body, anywhere
// in the text (including inside Markdown link targets). A trailing colon with
// nothing after it ("Note:") is not a URI, and single-letter schemes are left
// alone so Windows drive paths survive.
//
// This rule also turns Webex person mentions such as "<@personEmail:a@b.c>"
// and "<@personId:Y2lz...>" into "[blocked-url]", because "personEmail:" and
// "personId:" match as schemes. That is intended: outbound text cannot ping
// specific people through mention markup.
var outboundSchemeRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]+:\S+`)

// broadcastMentionRe matches the Webex "<@all>" broadcast mention,
// case-insensitively and with optional whitespace inside the brackets
// ("<@ all >").
var broadcastMentionRe = regexp.MustCompile(`(?i)<\s*@\s*all\s*>`)

// Sanitize neutralizes disallowed URL schemes and broadcast mentions in
// outbound text, leaving everything else, including whitespace, untouched.
// A URL whose scheme is not http, https or mailto (javascript:, data:,
// wmcp://, ...) becomes "[blocked-url]" in place, including inside a Markdown
// link target; "<@all>" becomes "[blocked-mention]".
func Sanitize(text string) string {
	text = broadcastMentionRe.ReplaceAllString(text, "[blocked-mention]")
	return outboundSchemeRe.ReplaceAllStringFunc(text, func(m string) string {
		scheme := m[:strings.IndexByte(m, ':')]
		if allowedOutboundProtocols[strings.ToLower(scheme)] {
			return m
		}
		return "[blocked-url]"
	})
}

// Prepare checks lengths on the raw input and sanitizes text and markdown.
// It returns ErrEmpty when required is set and both are empty, and a
// *TooLongError when either exceeds MaxMessageLen bytes before sanitizing.
func Prepare(text, markdown string, required bool) (string, string, error) {
	if required && text == "" && markdown == "" {
		return "", "", ErrEmpty
	}
	for _, s := range []string{text, markdown} {
		if len(s) > MaxMessageLen {
			return "", "", &TooLongError{Len: len(s)}
		}
	}
	return Sanitize(text), Sanitize(markdown), nil
}
