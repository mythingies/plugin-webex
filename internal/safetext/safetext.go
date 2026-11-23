// Package safetext holds the text-safety rules shared by the MCP tools and
// Reply Desk: wrapping inbound Webex text for an LLM, and checking outbound
// text before it is sent.
package safetext

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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
func Sandbox(text string) string {
	return "<external-message>" + wrapperTagRe.ReplaceAllString(text, "&lt;$1") + "</external-message>"
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
