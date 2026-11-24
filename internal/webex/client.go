package webex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// allowedRedirectPathPrefixes are the Webex API path prefixes allowed in redirects.
// This prevents a compromised DNS from steering the token to non-API paths.
var allowedRedirectPathPrefixes = []string{"/v1/"}

// defaultBaseURL is the production Webex REST API. Requests go to Client.baseURL,
// which is this unless NewClientWithBaseURL set another.
const defaultBaseURL = "https://webexapis.com/v1"

// maxResponseSize limits API response bodies to 50MB to prevent OOM.
const maxResponseSize = 50 * 1024 * 1024

// TokenProvider returns a valid Webex access token.
// Implementations handle static PATs, OAuth with auto-refresh, etc.
type TokenProvider interface {
	Token() (string, error)
}

// Client is a lightweight Webex REST API client.
type Client struct {
	tokenProvider TokenProvider
	httpClient    *http.Client
	baseURL       string                // API root without a trailing slash, e.g. https://webexapis.com/v1
	sleep         func(d time.Duration) // waits out a short Retry-After; tests replace it
}

// NewClient creates a new Webex API client with security-hardened defaults.
func NewClient(provider TokenProvider) *Client {
	return newClient(provider, defaultBaseURL)
}

func newClient(provider TokenProvider, base string) *Client {
	if provider == nil {
		panic("webex.NewClient: TokenProvider must not be nil")
	}

	return &Client{
		tokenProvider: provider,
		baseURL:       base,
		sleep:         time.Sleep,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				// Block redirects to different hosts to prevent token leakage.
				for _, prev := range via {
					if prev.URL.Host != req.URL.Host {
						return fmt.Errorf("redirect to different host blocked")
					}
				}
				// Block redirects to unexpected paths (e.g., /auth/login via DNS compromise).
				pathOK := false
				for _, prefix := range allowedRedirectPathPrefixes {
					if strings.HasPrefix(req.URL.Path, prefix) {
						pathOK = true
						break
					}
				}
				if !pathOK {
					return fmt.Errorf("redirect to unexpected path blocked: %s", req.URL.Path)
				}
				return nil
			},
		},
	}
}

// Space represents a Webex space (room).
type Space struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Type         string `json:"type"`
	IsLocked     bool   `json:"isLocked"`
	LastActivity string `json:"lastActivity"`
	Created      string `json:"created"`
}

// Message represents a Webex message.
type Message struct {
	ID          string   `json:"id"`
	RoomID      string   `json:"roomId"`
	RoomType    string   `json:"roomType"`
	Text        string   `json:"text"`
	PersonID    string   `json:"personId"`
	PersonEmail string   `json:"personEmail"`
	Created     string   `json:"created"`
	ParentID    string   `json:"parentId,omitempty"`
	Files       []string `json:"files,omitempty"`

	Markdown        string            `json:"markdown,omitempty"`
	Updated         string            `json:"updated,omitempty"` // present only when the author edited the message
	MentionedPeople []string          `json:"mentionedPeople,omitempty"`
	MentionedGroups []string          `json:"mentionedGroups,omitempty"`
	Attachments     []json.RawMessage `json:"attachments,omitempty"` // adaptive cards; content is not interpreted
	IsVoiceClip     bool              `json:"isVoiceClip,omitempty"`
}

// Person represents a Webex user.
type Person struct {
	ID          string   `json:"id"`
	Emails      []string `json:"emails"`
	DisplayName string   `json:"displayName"`
	NickName    string   `json:"nickName"`
	OrgID       string   `json:"orgId"`
	Created     string   `json:"created"`
	Status      string   `json:"status"`
	Type        string   `json:"type"`
}

// ListSpaces returns the user's Webex spaces.
func (c *Client) ListSpaces(max int) ([]Space, error) {
	params := url.Values{}
	if max > 0 {
		params.Set("max", fmt.Sprintf("%d", max))
	}
	params.Set("sortBy", "lastactivity")

	var result struct {
		Items []Space `json:"items"`
	}
	if err := c.get("/rooms", params, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// GetMessages returns messages from a space.
func (c *Client) GetMessages(roomID string, max int) ([]Message, error) {
	params := url.Values{}
	params.Set("roomId", roomID)
	if max > 0 {
		params.Set("max", fmt.Sprintf("%d", max))
	}

	var result struct {
		Items []Message `json:"items"`
	}
	if err := c.get("/messages", params, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// SendMessage sends a message to a space, person, or thread. When markdown is set,
// Webex renders it, and text is the fallback for clients without rich text.
func (c *Client) SendMessage(roomID, toPersonID, toPersonEmail, parentID, text, markdown string) (*Message, error) {
	if roomID == "" && toPersonID == "" && toPersonEmail == "" {
		return nil, fmt.Errorf("at least one recipient required (roomID, toPersonID, or toPersonEmail)")
	}
	if text == "" && markdown == "" {
		return nil, fmt.Errorf("text or markdown is required")
	}

	body := map[string]string{}
	if text != "" {
		body["text"] = text
	}
	if markdown != "" {
		body["markdown"] = markdown
	}
	if roomID != "" {
		body["roomId"] = roomID
	}
	if toPersonID != "" {
		body["toPersonId"] = toPersonID
	}
	if toPersonEmail != "" {
		body["toPersonEmail"] = toPersonEmail
	}
	if parentID != "" {
		body["parentId"] = parentID
	}

	var msg Message
	if err := c.post("/messages", body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// GetPerson returns a person's profile by ID.
func (c *Client) GetPerson(personID string) (*Person, error) {
	var person Person
	if err := c.get("/people/"+url.PathEscape(personID), nil, &person); err != nil {
		return nil, err
	}
	return &person, nil
}

// GetMe returns the authenticated user's profile.
func (c *Client) GetMe() (*Person, error) {
	var person Person
	if err := c.get("/people/me", nil, &person); err != nil {
		return nil, err
	}
	return &person, nil
}

// ListMembers returns members of a space.
func (c *Client) ListMembers(roomID string, max int) ([]Person, error) {
	params := url.Values{}
	params.Set("roomId", roomID)
	if max > 0 {
		params.Set("max", fmt.Sprintf("%d", max))
	}

	var result struct {
		Items []struct {
			PersonID      string `json:"personId"`
			PersonEmail   string `json:"personEmail"`
			PersonDisplay string `json:"personDisplayName"`
			IsModerator   bool   `json:"isModerator"`
		} `json:"items"`
	}
	if err := c.get("/memberships", params, &result); err != nil {
		return nil, err
	}

	people := make([]Person, len(result.Items))
	for i, m := range result.Items {
		people[i] = Person{
			ID:          m.PersonID,
			DisplayName: m.PersonDisplay,
			Emails:      []string{m.PersonEmail},
		}
	}
	return people, nil
}

// readLimitedBody reads a response body with a size limit.
func readLimitedBody(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, maxResponseSize))
}

// get performs an authenticated GET request on an API path such as "/rooms".
func (c *Client) get(path string, params url.Values, out interface{}) error {
	_, err := c.getHeaders(c.apiURL(path, params), out)
	return err
}

// apiURL joins an API path and its query to the client's base URL.
func (c *Client) apiURL(path string, params url.Values) string {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return u
}

// A GET answered 429 or 423 with a Retry-After of at most maxInlineRetryWait is
// retried up to maxRateLimitRetries times. A longer wait returns *RateLimitError.
const (
	maxRateLimitRetries = 2
	maxInlineRetryWait  = 30 * time.Second
)

// getHeaders performs an authenticated GET on u and decodes a 200 response into out,
// returning the response headers (for Link paging). u is a full URL: either built by
// apiURL, or a paging link the caller has checked with resolveNextLink. A short
// Retry-After on 429 or 423 is waited out in-line.
func (c *Client) getHeaders(u string, out interface{}) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		// Fetched per attempt, so a token refreshed during a wait is picked up.
		token, err := c.tokenProvider.Token()
		if err != nil {
			return nil, fmt.Errorf("getting access token: %w", err)
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.httpClient.Do(req) // #nosec G704 -- u is c.baseURL plus an API path, or a Link URL checked by resolveNextLink
		if err != nil {
			return nil, fmt.Errorf("executing request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(out)
			_ = resp.Body.Close()
			return resp.Header, err
		}

		err = responseError(resp)
		var rl *RateLimitError
		if errors.As(err, &rl) && rl.RetryAfter <= maxInlineRetryWait && attempt < maxRateLimitRetries {
			slog.Debug("webex API rate limited, retrying", "status", rl.Status, "retry_after", rl.RetryAfter, "attempt", attempt+1)
			c.sleep(rl.RetryAfter)
			continue
		}
		return nil, err
	}
}

// apiError is a non-2xx Webex response. Its message is the long-standing
// "webex API error (status N)"; a 404 also matches ErrNotFound with errors.Is.
type apiError struct {
	Status int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("webex API error (status %d)", e.Status)
}

func (e *apiError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// responseError reads and closes the body of a failed response, logs it at debug
// level, and returns the typed error for its status: *RateLimitError for 429 and
// 423, otherwise *apiError (which matches ErrNotFound for 404).
func responseError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	body, err := readLimitedBody(resp.Body)
	if err != nil {
		slog.Debug("failed to read error response body", "error", err)
	}
	slog.Debug("webex API error", "status", resp.StatusCode, "body", string(body))

	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusLocked:
		return &RateLimitError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	default:
		return &apiError{Status: resp.StatusCode}
	}
}

// defaultRetryAfter is the wait assumed when a 429 or 423 has no usable Retry-After.
const defaultRetryAfter = 5 * time.Second

// maxRetryAfterSeconds bounds a delta-seconds Retry-After so the Duration cannot overflow.
const maxRetryAfterSeconds = 1 << 30

// parseRetryAfter reads a Retry-After value, delta-seconds or an HTTP-date (RFC 9110
// section 10.2.3), relative to now. A missing or malformed value gives
// defaultRetryAfter; a date in the past gives 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultRetryAfter
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return defaultRetryAfter
		}
		return time.Duration(min(n, maxRetryAfterSeconds)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return defaultRetryAfter
}

// Attachment represents a Webex message attachment (e.g., Adaptive Card).
type Attachment struct {
	ContentType string      `json:"contentType"`
	Content     interface{} `json:"content"`
}

// SpaceAnalytics holds aggregated stats for a space.
type SpaceAnalytics struct {
	RoomID           string `json:"roomId"`
	RoomTitle        string `json:"roomTitle"`
	DaysBack         int    `json:"daysBack"`
	MessageCount     int    `json:"messageCount"`
	ActiveMembers    int    `json:"activeMembers"`
	TotalMembers     int    `json:"totalMembers"`
	PeakHour         int    `json:"peakHour"`
	MostActivePerson string `json:"mostActivePerson"`
}

// SendAdaptiveCard sends an Adaptive Card to a space or person.
// The card parameter should be the Adaptive Card JSON body (map or struct).
func (c *Client) SendAdaptiveCard(roomID, toPersonEmail string, card interface{}) (*Message, error) {
	body := map[string]interface{}{
		"text": "Adaptive Card", // fallback text for clients that don't support cards
		"attachments": []Attachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content:     card,
			},
		},
	}
	if roomID != "" {
		body["roomId"] = roomID
	}
	if toPersonEmail != "" {
		body["toPersonEmail"] = toPersonEmail
	}

	var msg Message
	if err := c.post("/messages", body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// MaxUploadSize is Webex's limit for the one file a message may carry.
const MaxUploadSize = 100 << 20

// uploadTimeout replaces the client's 30-second timeout for an upload, which may be up
// to MaxUploadSize.
const uploadTimeout = 5 * time.Minute

// ShareFile posts data as a file named name to a space or thread, with optional text
// or markdown, as multipart/form-data, as the Webex Go SDK's CreateWithAttachment does.
func (c *Client) ShareFile(roomID, parentID, name string, data []byte, text, markdown string) (*Message, error) {
	if roomID == "" {
		return nil, fmt.Errorf("roomID is required")
	}
	if len(data) > MaxUploadSize {
		return nil, fmt.Errorf("file is %d bytes, over the %d-byte limit", len(data), MaxUploadSize)
	}
	fields := [][2]string{{"roomId", roomID}}
	for _, f := range [][2]string{{"parentId", parentID}, {"text", text}, {"markdown", markdown}} {
		if f[1] != "" {
			fields = append(fields, f)
		}
	}
	body, contentType, err := multipartBody(fields, name, data)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenProvider.Token()
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/messages", body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)

	hc := *c.httpClient
	hc.Timeout = uploadTimeout
	resp, err := hc.Do(req) // #nosec G704 -- the URL is c.baseURL plus a constant path
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	var msg Message
	if err := decode(resp, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// multipartBody writes fields, then data as the "files" part named name.
func multipartBody(fields [][2]string, name string, data []byte) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", fmt.Errorf("writing field %s: %w", f[0], err)
		}
	}
	part, err := w.CreateFormFile("files", name)
	if err != nil {
		return nil, "", fmt.Errorf("creating file part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", fmt.Errorf("writing file part: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("closing multipart body: %w", err)
	}
	return &body, w.FormDataContentType(), nil
}

// GetSpaceAnalytics computes client-side analytics for a space over a time window.
func (c *Client) GetSpaceAnalytics(roomID string, daysBack int) (*SpaceAnalytics, error) {
	if daysBack <= 0 {
		daysBack = 7
	}

	// Fetch messages (up to 1000, API max).
	messages, err := c.GetMessages(roomID, 1000)
	if err != nil {
		return nil, fmt.Errorf("fetching messages: %w", err)
	}

	// Fetch members.
	members, err := c.ListMembers(roomID, 1000)
	if err != nil {
		return nil, fmt.Errorf("fetching members: %w", err)
	}

	// Fetch space details for title.
	spaces, err := c.ListSpaces(1000)
	if err != nil {
		return nil, fmt.Errorf("fetching spaces: %w", err)
	}
	var roomTitle string
	for _, sp := range spaces {
		if sp.ID == roomID {
			roomTitle = sp.Title
			break
		}
	}

	cutoff := time.Now().AddDate(0, 0, -daysBack)

	hourCounts := make(map[int]int)
	personCounts := make(map[string]int)
	activeSet := make(map[string]bool)
	messageCount := 0

	for _, msg := range messages {
		t, err := time.Parse(time.RFC3339, msg.Created)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			continue
		}
		messageCount++
		hourCounts[t.Hour()]++
		personCounts[msg.PersonEmail]++
		activeSet[msg.PersonEmail] = true
	}

	// Find peak hour.
	peakHour := 0
	peakCount := 0
	for h, c := range hourCounts {
		if c > peakCount {
			peakHour = h
			peakCount = c
		}
	}

	// Find most active person.
	var mostActive string
	mostActiveCount := 0
	for p, c := range personCounts {
		if c > mostActiveCount {
			mostActive = p
			mostActiveCount = c
		}
	}

	return &SpaceAnalytics{
		RoomID:           roomID,
		RoomTitle:        roomTitle,
		DaysBack:         daysBack,
		MessageCount:     messageCount,
		ActiveMembers:    len(activeSet),
		TotalMembers:     len(members),
		PeakHour:         peakHour,
		MostActivePerson: mostActive,
	}, nil
}

// Meeting represents a Webex meeting.
type Meeting struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	MeetingNumber   string `json:"meetingNumber"`
	MeetingType     string `json:"meetingType"`
	State           string `json:"state"`
	Start           string `json:"start"`
	End             string `json:"end"`
	Timezone        string `json:"timezone"`
	Agenda          string `json:"agenda"`
	HostEmail       string `json:"hostEmail"`
	HostDisplayName string `json:"hostDisplayName"`
	WebLink         string `json:"webLink"`
}

// Transcript represents a Webex meeting transcript.
type Transcript struct {
	ID              string `json:"id"`
	MeetingID       string `json:"meetingId"`
	MeetingTopic    string `json:"meetingTopic"`
	StartTime       string `json:"startTime"`
	VttDownloadLink string `json:"vttDownloadLink"`
	TxtDownloadLink string `json:"txtDownloadLink"`
	Status          string `json:"status"`
}

// ListMeetings returns meetings in a time range.
func (c *Client) ListMeetings(from, to string, max int) ([]Meeting, error) {
	params := url.Values{}
	if from != "" {
		params.Set("from", from)
	}
	if to != "" {
		params.Set("to", to)
	}
	if max > 0 {
		params.Set("max", fmt.Sprintf("%d", max))
	}

	var result struct {
		Items []Meeting `json:"items"`
	}
	if err := c.get("/meetings", params, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// ListTranscripts returns transcripts for a meeting.
func (c *Client) ListTranscripts(meetingID string, max int) ([]Transcript, error) {
	params := url.Values{}
	if meetingID != "" {
		params.Set("meetingId", meetingID)
	}
	if max > 0 {
		params.Set("max", fmt.Sprintf("%d", max))
	}

	var result struct {
		Items []Transcript `json:"items"`
	}
	if err := c.get("/meetingTranscripts", params, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// DownloadTranscript downloads the content of a transcript as text.
// Format should be "txt" or "vtt".
func (c *Client) DownloadTranscript(transcriptID, format string) (string, error) {
	if format == "" {
		format = "txt"
	}
	params := url.Values{}
	params.Set("format", format)

	u := c.baseURL + "/meetingTranscripts/" + url.PathEscape(transcriptID) + "/download?" + params.Encode()

	token, err := c.tokenProvider.Token()
	if err != nil {
		return "", fmt.Errorf("getting access token: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req) // #nosec G704 -- the URL is c.baseURL plus a constant path
	if err != nil {
		return "", fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := readLimitedBody(resp.Body)
		if err != nil {
			slog.Debug("failed to read error response body", "error", err)
		}
		slog.Debug("webex API error", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("webex API error (status %d)", resp.StatusCode)
	}

	body, err := readLimitedBody(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading transcript body: %w", err)
	}
	return string(body), nil
}

// post performs an authenticated POST request with JSON body.
func (c *Client) post(path string, body interface{}, out interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling body: %w", err)
	}

	token, err := c.tokenProvider.Token()
	if err != nil {
		return fmt.Errorf("getting access token: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	// Never retried, even on 429: a Webex POST has no idempotency key.
	resp, err := c.httpClient.Do(req) // #nosec G704 -- the URL is c.baseURL plus a constant path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	return decode(resp, out)
}

// decode closes resp and decodes a 200 or 201 response into out. Each caller runs Do
// itself on a URL built from c.baseURL, so the request target stays visibly constant.
// Failures are typed as in responseError (ErrNotFound, *RateLimitError), never retried.
func decode(resp *http.Response, out interface{}) error {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return responseError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(out)
}
