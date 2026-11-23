// Package replydesk finds direct messages the user has not answered, drafts
// replies with a tool-less Claude subprocess, and serves a loopback review UI
// where the user edits and approves each reply before it is sent.
//
// Authority is split (ADR 0002): detection and retrieval are Go code that
// only issues GETs; the drafter has no tools and returns JSON; only the send
// handler posts, and only after the user confirms the exact text.
//
// This file is the shared contract between the package's parts. Change it
// deliberately: detect.go, retrieve.go, drafter.go, store.go, the service and
// server, and web/app.js all depend on it.
package replydesk

import (
	"context"
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// API is the subset of *webex.Client that Reply Desk uses, so tests can fake
// it. Everything except SendMessage must be a GET.
type API interface {
	GetMe() (*webex.Person, error)
	ListDirectRooms(activeSince time.Time, limit int) ([]webex.Space, error)
	ListMessages(opts webex.ListMessagesOptions) ([]webex.Message, error)
	GetMessage(id string) (*webex.Message, error)
	SendMessage(roomID, toPersonID, toPersonEmail, parentID, text, markdown string) (*webex.Message, error)
}

var _ API = (*webex.Client)(nil)

const (
	// DefaultLookback is how far back a burst's newest message may be.
	DefaultLookback = 7 * 24 * time.Hour
	// ContextMessages is how many messages before the burst are kept for context.
	ContextMessages = 8
	// MaxBurst caps the messages in one burst; longer bursts are truncated to
	// the newest MaxBurst and flagged FlagLongBurst.
	MaxBurst = 30
	// FillMarker opens a placeholder the user must replace before sending.
	FillMarker = "[[FILL"
)

// Flags on a Pending item.
const (
	FlagAttachment = "attachment" // a burst message has files
	FlagCard       = "card"       // a burst message has an adaptive card
	FlagVoiceClip  = "voice_clip" // a burst message is a voice clip
	FlagEdited     = "edited"     // a burst message was edited
	FlagLongBurst  = "long_burst" // burst truncated to MaxBurst
	FlagNoHistory  = "no_history" // no message from the user found in fetched history
)

// Pending is one DM room whose newest messages are from the other person.
type Pending struct {
	RoomID    string
	RoomTitle string // for a DM, the other person's display name
	// Burst is the other person's messages since the user's last message,
	// oldest first. Never empty.
	Burst []webex.Message
	// Context is up to ContextMessages messages before the burst, oldest
	// first, from either side.
	Context []webex.Message
	// HeadID and HeadUpdated identify the newest burst message. Per-room state
	// is keyed on HeadID, so a new message re-opens a dismissed room.
	HeadID      string
	HeadUpdated string
	HeadCreated time.Time
	// Since is the created time of the oldest burst message.
	Since time.Time
	// ParentID is the thread to reply in ("" means top level): set when the
	// newest burst message is a thread reply.
	ParentID string
	Flags    []string
	// Reason is a one-line, human-readable explanation of why the room is
	// listed, e.g. "3 messages since your last reply on 2026-11-20 14:02".
	Reason string
}

// RefMsg is a message as the drafter sees it: a local ref instead of a Webex
// ID. Refs are "b1".. for the burst, "c1".. for context, "p1".. for prior
// messages, numbered in slice order (Burst and Context oldest first, Prior
// best match first).
type RefMsg struct {
	Ref     string
	FromMe  bool
	Created string // RFC 3339
	Text    string // raw text; the drafter applies safetext.Sandbox
}

// DraftInput is everything the drafter is given. Nothing else reaches it.
type DraftInput struct {
	AskerName     string
	Burst         []RefMsg
	Context       []RefMsg
	Prior         []RefMsg // the user's own earlier messages in this DM, best match first
	Instruction   string   // the user's regenerate instruction, if any
	PreviousDraft string   // the draft being regenerated, if any
}

// DraftOutput is the drafter's validated JSON.
type DraftOutput struct {
	NeedsReply       bool     `json:"needs_reply"`
	NeedsReplyReason string   `json:"needs_reply_reason"`
	Summary          string   `json:"summary"`
	Asks             []string `json:"asks"`
	MissingInfo      []string `json:"missing_info"`
	Draft            string   `json:"draft"`
	CitedRefs        []string `json:"cited_refs"`
	NoPriorAnswer    bool     `json:"no_prior_answer"`
}

// Drafter turns a DraftInput into a DraftOutput.
type Drafter interface {
	Draft(ctx context.Context, in DraftInput) (*DraftOutput, error)
	// Mode is "bare", "isolated", or "off".
	Mode() string
}

// Drafter modes.
const (
	ModeAuto     = "auto"
	ModeBare     = "bare"
	ModeIsolated = "isolated"
	ModeOff      = "off"
)

// Backend is what the HTTP server needs. *Service implements it; server
// tests use a fake.
type Backend interface {
	Status() StatusView
	// Items lists inbox items; all=false hides sent and dismissed rooms.
	Items(all bool) []ItemView
	// Rescan starts a scan in the background and returns immediately.
	Rescan(ctx context.Context)
	// Draft starts (re)drafting one room in the background.
	Draft(ctx context.Context, roomID, instruction string) *APIError
	// Send runs the pre-send checks and sends. On failure it returns the HTTP
	// status and error body.
	Send(ctx context.Context, req SendRequest) (messageID string, status int, apiErr *APIError)
	Dismiss(roomID, reason string) *APIError
	Snooze(roomID string, hours int) *APIError
}

// State is a room's Reply Desk state for one burst head.
type State string

const (
	StatePending     State = "pending"
	StateDrafting    State = "drafting"
	StateDrafted     State = "drafted"
	StateDraftFailed State = "draft_failed"
	StateSending     State = "sending"
	StateSent        State = "sent"
	StateSendUnknown State = "send_unknown" // crashed or errored mid-send; never retried automatically
	StateDismissed   State = "dismissed"
	StateSnoozed     State = "snoozed"
)

// Dismiss reasons.
const (
	DismissNoReplyNeeded  = "no_reply_needed"
	DismissRepliedElsewhr = "replied_elsewhere"
	DismissReacted        = "reacted"
	DismissOther          = "other"
)

// Citation is one of the user's prior messages a draft relied on.
type Citation struct {
	MessageID string `json:"message_id"`
	Created   string `json:"created"`
	Text      string `json:"text"` // capped at MaxCitationLen bytes
}

// MaxCitationLen caps citation excerpts kept at rest.
const MaxCitationLen = 500

// Draft is a stored draft for one burst head.
type Draft struct {
	HeadID           string     `json:"head_id"`
	Text             string     `json:"text"`
	Summary          string     `json:"summary"`
	Asks             []string   `json:"asks"`
	MissingInfo      []string   `json:"missing_info"`
	NeedsReply       bool       `json:"needs_reply"`
	NeedsReplyReason string     `json:"needs_reply_reason"`
	NoPriorAnswer    bool       `json:"no_prior_answer"`
	Cited            []Citation `json:"cited"`
	Instruction      string     `json:"instruction,omitempty"`
	Mode             string     `json:"mode"`
	CreatedAt        time.Time  `json:"created_at"`
}

// Record is the persisted state of one DM room.
type Record struct {
	RoomID string `json:"room_id"`
	HeadID string `json:"head_id"` // the burst head this state applies to
	// HeadCreated is the head's created time. A different head re-opens a
	// dismissed, sent, or snoozed room only if it is newer than this, so
	// deleting the newest message does not bring a room back.
	HeadCreated   time.Time `json:"head_created,omitempty"`
	State         State     `json:"state"`
	Draft         *Draft    `json:"draft,omitempty"`
	DismissReason string    `json:"dismiss_reason,omitempty"`
	SnoozeUntil   time.Time `json:"snooze_until,omitempty"`
	SentMessageID string    `json:"sent_message_id,omitempty"`
	SentSHA256    string    `json:"sent_sha256,omitempty"`
	Error         string    `json:"error,omitempty"` // last error shown to the user; never contains secrets
	UpdatedAt     time.Time `json:"updated_at"`
}

// ---- JSON views served to web/app.js ----

// MsgView is a message as the UI shows it. Text is raw; the UI must render it
// with textContent, never innerHTML.
type MsgView struct {
	ID        string `json:"id"`
	FromMe    bool   `json:"from_me"`
	Author    string `json:"author"`
	Created   string `json:"created"`
	Text      string `json:"text"`
	InThread  bool   `json:"in_thread"`
	Files     int    `json:"files"`
	Card      bool   `json:"card"`
	VoiceClip bool   `json:"voice_clip"`
	Edited    bool   `json:"edited"`
}

// CitationView is a citation as the UI shows it.
type CitationView struct {
	Created string `json:"created"`
	Text    string `json:"text"`
}

// DraftView is a draft as the UI shows it.
type DraftView struct {
	Text             string         `json:"text"`
	Summary          string         `json:"summary"`
	Asks             []string       `json:"asks"`
	MissingInfo      []string       `json:"missing_info"`
	NeedsReply       bool           `json:"needs_reply"`
	NeedsReplyReason string         `json:"needs_reply_reason"`
	NoPriorAnswer    bool           `json:"no_prior_answer"`
	Cited            []CitationView `json:"cited"`
	Instruction      string         `json:"instruction,omitempty"`
	Mode             string         `json:"mode"`
	CreatedAt        string         `json:"created_at"`
}

// ItemView is one room in the inbox.
type ItemView struct {
	RoomID      string     `json:"room_id"`
	HeadID      string     `json:"head_id"` // the burst head the UI is showing; echoed in SendRequest
	Title       string     `json:"title"`
	State       State      `json:"state"`
	Since       string     `json:"since"`
	HeadCreated string     `json:"head_created"`
	Reason      string     `json:"reason"`
	Flags       []string   `json:"flags"`
	InThread    bool       `json:"in_thread"`
	Burst       []MsgView  `json:"burst"`
	Context     []MsgView  `json:"context"`
	Draft       *DraftView `json:"draft,omitempty"`
	// DraftStale is true when the draft was written for an older burst head.
	DraftStale  bool   `json:"draft_stale"`
	SnoozeUntil string `json:"snooze_until,omitempty"`
	Error       string `json:"error,omitempty"`
}

// StatusView is the status bar.
type StatusView struct {
	Me            string `json:"me"` // the user's display name
	DrafterMode   string `json:"drafter_mode"`
	LastScan      string `json:"last_scan,omitempty"`
	Scanning      bool   `json:"scanning"`
	Drafting      int    `json:"drafting"` // drafts in flight
	Lookback      string `json:"lookback"`
	Error         string `json:"error,omitempty"`
	MaxMessageLen int    `json:"max_message_len"`
}

// SendRequest is POST /api/send.
type SendRequest struct {
	RoomID       string `json:"room_id"`
	HeadID       string `json:"head_id"` // ItemView.HeadID the user was looking at; a mismatch is new_messages
	Text         string `json:"text"`
	TextSHA256   string `json:"text_sha256"`   // hex sha256 of Text as confirmed by the user
	AckSanitized bool   `json:"ack_sanitized"` // the user saw and accepted the sanitized version
}

// APIError is the JSON body of a 4xx/5xx response.
type APIError struct {
	Code          string `json:"code"`
	Detail        string `json:"detail"`
	SanitizedText string `json:"sanitized_text,omitempty"` // with code "sanitized"
}

// Error codes returned by the send endpoint.
const (
	CodeAlreadyReplied = "already_replied" // 409: the user replied after the burst
	CodeNewMessages    = "new_messages"    // 409: the other person wrote again since the scan
	CodeAskDeleted     = "ask_deleted"     // 409: the newest burst message was deleted
	CodeAskEdited      = "ask_edited"      // 409: the newest burst message was edited since the scan
	CodeSanitized      = "sanitized"       // 409: Sanitize changed the text; resend with ack_sanitized
	CodeFillRemaining  = "fill_placeholders"
	CodeTooLong        = "too_long"
	CodeEmpty          = "empty"
	CodeHashMismatch   = "hash_mismatch"
	CodeNotPending     = "not_pending" // unknown room, or already sent/dismissed
	CodeRateLimited    = "rate_limited"
	CodeSendUnknown    = "send_unknown"
	CodeBusy           = "busy"        // 409: a draft is already running and the request carries an instruction
	CodeDrafterOff     = "drafter_off" // 400: drafting is unavailable
	CodeSignedOut      = "signed_out"  // 409: --logout/--switch ran while this process was open
)
