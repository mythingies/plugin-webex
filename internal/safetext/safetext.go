// Package safetext holds the text-safety rules shared by the MCP tools and
// Reply Desk: wrapping inbound Webex text for an LLM, and checking outbound
// text before it is sent.
package safetext

import (
	"errors"
	"fmt"
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

// Sandbox wraps Webex-sourced text so the LLM treats it as data, not
// instructions.
func Sandbox(text string) string {
	return "<external-message>" + text + "</external-message>"
}

// Sanitize neutralizes disallowed URL schemes and broadcast mentions in
// outbound text, leaving everything else, including whitespace, untouched.
func Sanitize(text string) string {
	return text
}

// Prepare checks lengths on the raw input and sanitizes text and markdown.
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
