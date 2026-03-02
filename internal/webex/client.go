package webex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
