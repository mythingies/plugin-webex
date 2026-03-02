package webex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://webexapis.com/v1"

// Client is a lightweight Webex REST API client using a Personal Access Token.
type Client struct {
	token      string
	httpClient *http.Client
}

// NewClient creates a new Webex API client.
func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{},
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

// SendMessage sends a message to a space, person, or thread.
func (c *Client) SendMessage(roomID, toPersonID, toPersonEmail, parentID, text string) (*Message, error) {
	body := map[string]string{"text": text}
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
	if err := c.get("/people/"+personID, nil, &person); err != nil {
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

// get performs an authenticated GET request.
func (c *Client) get(path string, params url.Values, out interface{}) error {
	u := baseURL + path
	if params != nil {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webex API error %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
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

// ShareFile is a placeholder for file upload/share (multipart upload deferred).
func (c *Client) ShareFile(roomID, filePath string) error {
	return fmt.Errorf("share_file is not yet implemented (multipart upload deferred to a future version)")
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

	u := baseURL + "/meetingTranscripts/" + transcriptID + "/download?" + params.Encode()

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("webex API error %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading transcript body: %w", err)
	}
	return string(body), nil
}

// Token returns the client's auth token (used by the listener).
func (c *Client) Token() string {
	return c.token
}

// post performs an authenticated POST request with JSON body.
func (c *Client) post(path string, body interface{}, out interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webex API error %d: %s", resp.StatusCode, string(respBody))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}
