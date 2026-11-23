package replydesk

import (
	"fmt"
	"strings"
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// botDomains are the email domains Webex assigns to bot accounts.
var botDomains = []string{"webex.bot", "sparkbot.io"}

// flagOrder is the order flags appear in Pending.Flags: the order the
// constants are declared in types.go.
var flagOrder = []string{
	FlagAttachment,
	FlagCard,
	FlagVoiceClip,
	FlagEdited,
	FlagLongBurst,
	FlagNoHistory,
}

// Detect returns the pending burst in one DM room, or nil when the room is
// answered, belongs to a bot, or its newest burst message is older than
// lookback. newestFirst is the room's fetched history, newest first; me is
// the user's person ID. Detect is pure: no I/O.
//
// The burst is the leading run of newestFirst not sent by me. Context is the
// ContextMessages messages that follow the whole (untruncated) burst, so it
// starts with the user's last reply when there is one. Reason counts the
// whole burst, even when Burst itself was truncated to MaxBurst.
func Detect(me string, room webex.Space, newestFirst []webex.Message, now time.Time, lookback time.Duration) *Pending {
	if room.Type != "" && room.Type != "direct" {
		return nil
	}

	n := 0
	for n < len(newestFirst) && newestFirst[n].PersonID != me {
		n++
	}
	if n == 0 {
		return nil
	}
	full := newestFirst[:n]
	for i := range full {
		if IsBot(full[i].PersonEmail) {
			return nil
		}
	}

	head := full[0]
	headCreated, ok := parseTime(head.Created)
	if !ok {
		return nil
	}
	if now.Sub(headCreated) > lookback {
		return nil
	}

	set := make(map[string]bool, len(flagOrder))
	kept := full
	if len(kept) > MaxBurst {
		kept = kept[:MaxBurst]
		set[FlagLongBurst] = true
	}
	for i := range kept {
		m := &kept[i]
		if len(m.Files) > 0 {
			set[FlagAttachment] = true
		}
		if len(m.Attachments) > 0 {
			set[FlagCard] = true
		}
		if m.IsVoiceClip {
			set[FlagVoiceClip] = true
		}
		if m.Updated != "" {
			set[FlagEdited] = true
		}
	}

	rest := newestFirst[n:]
	hasMine := len(rest) > 0 // rest, when non-empty, starts with my last reply
	if !hasMine {
		set[FlagNoHistory] = true
	}

	ctxEnd := min(len(rest), ContextMessages)

	burst := reversed(kept)
	since, _ := parseTime(burst[0].Created)

	p := &Pending{
		RoomID:      room.ID,
		RoomTitle:   room.Title,
		Burst:       burst,
		Context:     reversed(rest[:ctxEnd]),
		HeadID:      head.ID,
		HeadUpdated: head.Updated,
		HeadCreated: headCreated,
		Since:       since,
		ParentID:    head.ParentID,
		Flags:       orderedFlags(set),
	}

	count := plural(n)
	switch {
	case !hasMine:
		p.Reason = count + "; no reply from you in the fetched history"
	default:
		if t, ok := parseTime(rest[0].Created); ok {
			p.Reason = fmt.Sprintf("%s since your last reply on %s", count, t.In(now.Location()).Format("2006-01-02 15:04"))
		} else {
			p.Reason = count + " since your last reply"
		}
	}
	return p
}

// IsBot reports whether a sender email belongs to a Webex bot: its domain is
// webex.bot or sparkbot.io, or a subdomain of either. The check is
// case-insensitive.
func IsBot(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(email[at+1:])), ".")
	for _, d := range botDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// parseTime parses a Webex timestamp: RFC 3339 with or without fractional
// seconds.
func parseTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// reversed returns a reversed copy of msgs, never nil.
func reversed(msgs []webex.Message) []webex.Message {
	out := make([]webex.Message, len(msgs))
	for i := range msgs {
		out[len(msgs)-1-i] = msgs[i]
	}
	return out
}

// orderedFlags returns the set flags in declaration order. It never returns
// nil, so an item with no flags marshals as [] rather than null.
func orderedFlags(set map[string]bool) []string {
	out := []string{}
	for _, f := range flagOrder {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}

// plural formats a message count: "1 message", "3 messages".
func plural(n int) string {
	if n == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", n)
}
