package replydesk

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// Grounding limits: how much older history of the DM is searched for the
// user's own earlier messages.
const (
	groundPageSize    = 100
	groundMaxPages    = 3
	groundMaxMessages = 300
	groundMaxAge      = 180 * 24 * time.Hour
	// groundPriorK is how many prior messages are given to the drafter.
	groundPriorK = 5
)

// buildDraftInput assembles what the drafter sees for p, and the map from
// local refs back to messages. Grounding is best effort: a failed history
// fetch is logged and drafting goes on with what was fetched.
func (s *Service) buildDraftInput(ctx context.Context, p *Pending, instruction, previous string) (DraftInput, map[string]webex.Message) {
	refs := make(map[string]webex.Message, len(p.Burst)+len(p.Context)+groundPriorK)
	in := DraftInput{
		AskerName:     p.RoomTitle,
		Burst:         s.refMsgs("b", p.Burst, refs),
		Context:       s.refMsgs("c", p.Context, refs),
		Instruction:   instruction,
		PreviousDraft: previous,
	}

	query := make([]string, 0, len(in.Burst))
	for _, m := range in.Burst {
		query = append(query, m.Text)
	}
	mine := s.priorMessages(ctx, p)
	in.Prior = s.refMsgs("p", Rank(strings.Join(query, "\n"), mine, groundPriorK), refs)
	return in, refs
}

// priorMessages returns the user's own messages in p's room older than the
// context (up to groundMaxPages pages, groundMaxMessages messages, and
// groundMaxAge old). Messages in the burst or context are left out: the
// drafter already sees them under b/c refs, and a p ref for the same message
// would let one message be cited twice.
func (s *Service) priorMessages(ctx context.Context, p *Pending) []webex.Message {
	var mine []webex.Message
	shown := make(map[string]bool, len(p.Burst)+len(p.Context))
	for _, ms := range [][]webex.Message{p.Burst, p.Context} {
		for i := range ms {
			if ms[i].ID != "" {
				shown[ms[i].ID] = true
			}
		}
	}

	before := ""
	switch {
	case len(p.Context) > 0:
		before = p.Context[0].ID // oldest first
	case len(p.Burst) > 0:
		before = p.Burst[0].ID
	}
	if before == "" {
		return mine
	}
	cutoff := s.now().Add(-groundMaxAge)
	fetched := 0
	for page := 0; page < groundMaxPages && fetched < groundMaxMessages; page++ {
		if ctx.Err() != nil {
			break
		}
		msgs, err := s.api.ListMessages(webex.ListMessagesOptions{RoomID: p.RoomID, BeforeMessage: before, Max: groundPageSize})
		if err != nil {
			s.noteAuthErr(err)
			slog.Warn("replydesk: grounding history fetch failed; drafting with what was fetched", "room_id", p.RoomID, "err", err)
			break
		}
		if len(msgs) == 0 {
			break
		}
		tooOld := false
		for i := range msgs {
			if fetched >= groundMaxMessages {
				break
			}
			if t, ok := parseTime(msgs[i].Created); ok && t.Before(cutoff) {
				tooOld = true
				break
			}
			fetched++
			if msgs[i].PersonID == s.me && !shown[msgs[i].ID] {
				mine = append(mine, msgs[i])
			}
		}
		next := msgs[len(msgs)-1].ID
		if tooOld || next == "" || next == before {
			break
		}
		before = next
	}
	return mine
}

// refMsgs converts msgs to drafter messages with refs prefix1.., recording
// each ref in refs.
func (s *Service) refMsgs(prefix string, msgs []webex.Message, refs map[string]webex.Message) []RefMsg {
	out := make([]RefMsg, 0, len(msgs))
	for i := range msgs {
		ref := fmt.Sprintf("%s%d", prefix, i+1)
		refs[ref] = msgs[i]
		out = append(out, RefMsg{
			Ref:     ref,
			FromMe:  msgs[i].PersonID == s.me,
			Created: fmtStamp(msgs[i].Created),
			Text:    refText(&msgs[i]),
		})
	}
	return out
}

// refText is a message's text, or a placeholder for a message without text.
func refText(m *webex.Message) string {
	if t := messageText(m); strings.TrimSpace(t) != "" {
		return t
	}
	// A voice clip is also a file, so it is checked first.
	switch {
	case m.IsVoiceClip:
		return "[voice clip]"
	case len(m.Attachments) > 0:
		return "[adaptive card]"
	case len(m.Files) > 0:
		return fmt.Sprintf("[shared %d file(s)]", len(m.Files))
	}
	return ""
}
