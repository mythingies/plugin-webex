package webex

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when the API answers 404, for example GetMessage on
// a deleted message.
var ErrNotFound = errors.New("webex: not found")

// RateLimitError is returned when the API answers 429 or 423 with a
// Retry-After longer than the client is willing to wait in-line.
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("webex API rate limited (status %d), retry after %s", e.Status, e.RetryAfter)
}

// ListMessagesOptions are the query parameters of GET /messages. RoomID is
// required. Results are newest first.
type ListMessagesOptions struct {
	RoomID        string
	ParentID      string    // list replies to this thread root
	BeforeMessage string    // page backwards from this message ID (exclusive)
	Before        time.Time // only messages created before this time
	MentionedMe   bool      // mentionedPeople=me
	Max           int       // 1-1000; 0 means the API default (50)
}

// Page sizes for GET /messages. The API caps max at 1000, and at 100 when
// filtering by mentionedPeople. A 504 is retried once at half the page size, but
// not below minMessagesPageOn504.
const (
	maxMessagesPage          = 1000
	maxMentionedMessagesPage = 100
	defaultMessagesPage      = 50
	minMessagesPageOn504     = 10
)

// ListMessages returns one page of messages in a room, newest first.
func (c *Client) ListMessages(opts ListMessagesOptions) ([]Message, error) {
	if opts.RoomID == "" {
		return nil, fmt.Errorf("roomID is required")
	}

	limit := 0 // 0 omits max, so the API default applies
	if opts.Max > 0 {
		limit = min(opts.Max, maxMessagesPage)
	}
	if opts.MentionedMe && limit > maxMentionedMessagesPage {
		limit = maxMentionedMessagesPage
	}

	params := url.Values{}
	params.Set("roomId", opts.RoomID)
	if opts.ParentID != "" {
		params.Set("parentId", opts.ParentID)
	}
	if opts.BeforeMessage != "" {
		params.Set("beforeMessage", opts.BeforeMessage)
	}
	if !opts.Before.IsZero() {
		params.Set("before", opts.Before.UTC().Format(time.RFC3339))
	}
	if opts.MentionedMe {
		params.Set("mentionedPeople", "me")
	}

	list := func(limit int) ([]Message, error) {
		if limit > 0 {
			params.Set("max", strconv.Itoa(limit))
		}
		var result struct {
			Items []Message `json:"items"`
		}
		if err := c.get("/messages", params, &result); err != nil {
			return nil, err
		}
		return result.Items, nil
	}

	msgs, err := list(limit)
	var ae *apiError
	if err == nil || !errors.As(err, &ae) || ae.Status != http.StatusGatewayTimeout {
		return msgs, err
	}
	// A 504 on GET /messages usually means the page was too expensive to build.
	cur := limit
	if cur == 0 {
		cur = defaultMessagesPage
	}
	smaller := min(max(cur/2, minMessagesPageOn504), cur)
	slog.Debug("webex GET /messages timed out, retrying with a smaller page", "max", smaller)
	return list(smaller)
}

// Paging limits for GET /rooms.
const (
	maxRoomsPage  = 1000
	maxRoomsPages = 50
)

// errUnsafeNextLink is returned when a Link rel="next" URL leaves the API base URL.
var errUnsafeNextLink = errors.New("webex: refusing to follow a paging link outside the API base URL")

// ListDirectRooms returns the user's 1:1 rooms, most recently active first,
// following Link-header paging until a room's lastActivity is before
// activeSince (when non-zero) or limit rooms have been collected (when > 0).
//
// Rooms whose lastActivity cannot be parsed are skipped when activeSince is
// set. Paging stops after maxRoomsPages pages. A next link that is not on the
// client's base URL is not followed: the rooms so far are returned with an
// error, so the bearer token never goes to another host.
func (c *Client) ListDirectRooms(activeSince time.Time, limit int) ([]Space, error) {
	pageSize := maxRoomsPage
	if limit > 0 && limit < pageSize {
		pageSize = limit
	}
	params := url.Values{}
	params.Set("type", "direct")
	params.Set("sortBy", "lastactivity")
	params.Set("max", strconv.Itoa(pageSize))

	var rooms []Space
	target := c.apiURL("/rooms", params)
	for page := 0; page < maxRoomsPages; page++ {
		var result struct {
			Items []Space `json:"items"`
		}
		header, err := c.getHeaders(target, &result)
		if err != nil {
			return rooms, err
		}

		for _, room := range result.Items {
			if room.Type != "" && room.Type != "direct" {
				continue
			}
			if !activeSince.IsZero() {
				last, err := time.Parse(time.RFC3339, room.LastActivity)
				if err != nil {
					continue
				}
				if last.Before(activeSince) {
					return rooms, nil // sorted by last activity, so the rest are older
				}
			}
			rooms = append(rooms, room)
			if limit > 0 && len(rooms) >= limit {
				return rooms, nil
			}
		}

		ref := nextLink(header.Values("Link"))
		if ref == "" {
			return rooms, nil
		}
		next, ok := c.resolveNextLink(target, ref)
		if !ok {
			slog.Warn("webex: not following a paging link outside the API base URL")
			return rooms, errUnsafeNextLink
		}
		target = next
	}
	slog.Warn("webex: stopped paging direct rooms at the page limit", "pages", maxRoomsPages, "rooms", len(rooms))
	return rooms, nil
}

// resolveNextLink resolves a Link URL reference against the page it came from and
// reports whether it is safe to send the bearer token to: same scheme and host as
// the client's base URL, a path under the base path, no userinfo, and no dot
// segments (including percent-encoded ones).
func (c *Client) resolveNextLink(current, ref string) (string, bool) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", false
	}
	cur, err := url.Parse(current)
	if err != nil {
		return "", false
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	next := cur.ResolveReference(r)
	next.Fragment, next.RawFragment = "", ""

	if !strings.EqualFold(next.Scheme, base.Scheme) || !strings.EqualFold(next.Host, base.Host) || next.User != nil {
		return "", false
	}
	for _, seg := range strings.Split(next.Path, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	basePath := strings.TrimSuffix(base.Path, "/")
	if next.Path != basePath && !strings.HasPrefix(next.Path, basePath+"/") {
		return "", false
	}
	return next.String(), true
}

// nextLink returns the URL reference of the first rel="next" link in the given
// Link header values (RFC 8288, formerly RFC 5988), or "" if there is none.
func nextLink(values []string) string {
	for _, v := range values {
		for {
			v = strings.TrimLeft(v, " \t,")
			if !strings.HasPrefix(v, "<") {
				break
			}
			end := strings.IndexByte(v, '>')
			if end < 0 {
				break
			}
			ref := v[1:end]
			var params string
			params, v = cutOutsideQuotes(v[end+1:], ',')
			if relIsNext(params) {
				return strings.TrimSpace(ref)
			}
		}
	}
	return ""
}

// cutOutsideQuotes splits s at the first sep that is not inside a quoted string.
// The separator stays at the start of the rest.
func cutOutsideQuotes(s string, sep byte) (string, string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case inQuote && s[i] == '\\':
			i++ // skip the escaped character
		case s[i] == '"':
			inQuote = !inQuote
		case !inQuote && s[i] == sep:
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// relIsNext reports whether link parameters such as `; rel="next"` include the
// relation type "next". rel may be quoted or bare and may list several types.
func relIsNext(params string) bool {
	for params != "" {
		var p string
		p, params = cutOutsideQuotes(strings.TrimPrefix(params, ";"), ';')
		name, value, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		for _, rel := range strings.Fields(value) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}

// GetMessage returns one message by ID, or ErrNotFound if it was deleted.
func (c *Client) GetMessage(id string) (*Message, error) {
	if id == "" {
		return nil, fmt.Errorf("message ID is required")
	}
	var msg Message
	if err := c.get("/messages/"+url.PathEscape(id), nil, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// NewClientWithBaseURL is NewClient against a different API base URL, for
// tests that point the client at an httptest server.
func NewClientWithBaseURL(provider TokenProvider, base string) *Client {
	return newClient(provider, strings.TrimRight(base, "/"))
}
