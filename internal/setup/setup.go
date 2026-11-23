package setup

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

//go:embed setup.html
var setupHTML embed.FS

const version = "v0.9.5"

// Run starts the setup UI on a random localhost port and opens the browser.
func Run(binaryPath string) error {
	mux := http.NewServeMux()

	tmpl, err := template.ParseFS(setupHTML, "setup.html")
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, map[string]string{"Version": version}); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/api/detect", handleDetect)
	mux.HandleFunc("/api/pat", handlePAT)
	mux.HandleFunc("/api/oauth", handleOAuth(binaryPath))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding to localhost: %w", err)
	}

	addr := ln.Addr().String()
	url := "http://" + addr
	fmt.Fprintf(os.Stderr, "Setup UI available at: %s\n", url)

	go openBrowser(url)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 300 * time.Second,
	}
	return srv.Serve(ln)
}

type detectResponse struct {
	Mode     string `json:"mode"`                // "oauth", "pat", or ""
	ClientID string `json:"client_id,omitempty"` // masked
	Message  string `json:"message,omitempty"`
}

func handleDetect(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(os.Getenv("WEBEX_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("WEBEX_CLIENT_SECRET"))
	token := strings.TrimSpace(os.Getenv("WEBEX_TOKEN"))

	// Env-set credentials override keychain (for CI/testing). Otherwise
	// check the keychain for a stored client secret matching the env-set
	// client_id.
	switch {
	case clientID != "" && clientSecret != "":
		writeJSON(w, detectResponse{
			Mode:     "oauth",
			ClientID: maskValue(clientID),
			Message:  "OAuth credentials loaded from environment",
		})
	case clientID != "":
		if _, err := auth.LoadClientSecret(clientID); err == nil {
			writeJSON(w, detectResponse{
				Mode:     "oauth",
				ClientID: maskValue(clientID),
				Message:  "OAuth credentials loaded (client_id from environment, secret from keychain)",
			})
			return
		}
		writeJSON(w, detectResponse{})
	case token != "":
		writeJSON(w, detectResponse{
			Mode:    "pat",
			Message: "Personal Access Token loaded from environment",
		})
	default:
		writeJSON(w, detectResponse{})
	}
}

type patRequest struct {
	Token string `json:"token"`
}

type apiResponse struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Config      string `json:"config,omitempty"`
	Error       string `json:"error,omitempty"`
	AuthURL     string `json:"auth_url,omitempty"`
}

func handlePAT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSON(w, apiResponse{Error: "failed to read request"})
		return
	}

	var req patRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, apiResponse{Error: "invalid request"})
		return
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeJSON(w, apiResponse{Error: "token is required"})
		return
	}

	name, email, err := validateToken(token)
	if err != nil {
		writeJSON(w, apiResponse{Error: fmt.Sprintf("Token validation failed: %v", err)})
		return
	}

	config, err := saveMCPConfig(patEnv(token))
	if err != nil {
		writeJSON(w, apiResponse{Error: fmt.Sprintf("Failed to save config: %v", err)})
		return
	}

	writeJSON(w, apiResponse{
		DisplayName: name,
		Email:       email,
		Config:      config,
	})
}

type oauthRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// Popup means the page opened its own window for sign-in. The server then
	// streams the authorization URL back as the first NDJSON line instead of
	// opening the system browser, and the final result follows as the last.
	Popup bool `json:"popup"`
}

func handleOAuth(binaryPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			writeJSON(w, apiResponse{Error: "failed to read request"})
			return
		}

		var req oauthRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, apiResponse{Error: "invalid request"})
			return
		}

		clientID := strings.TrimSpace(req.ClientID)
		clientSecret := strings.TrimSpace(req.ClientSecret)

		if clientID == "" || clientSecret == "" {
			writeJSON(w, apiResponse{Error: "Client ID and Client Secret are required"})
			return
		}

		// Register the wmcp:// handler so the OAuth redirect reaches us.
		// Without it the callback never arrives and Authorize waits out its
		// timeout, so say why up front.
		if binaryPath != "" {
			if err := auth.RegisterProtocol(binaryPath); err != nil {
				slog.Warn("could not register wmcp:// protocol handler; the OAuth callback may not arrive", "error", err)
			}
		}

		cfg := auth.OAuthConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
		}

		provider, err := auth.NewOAuthProvider(cfg)
		if err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("OAuth setup failed: %v", err)})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		authorize := provider.Authorize
		if req.Popup {
			authorize = func(ctx context.Context) error {
				return provider.AuthorizeWith(ctx, func(authURL string) error {
					writeJSON(w, apiResponse{AuthURL: authURL})
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return nil
				})
			}
		}

		if err := authorize(ctx); err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("Authorization failed: %v", err)})
			return
		}

		// Persist the client secret to the OS keychain (or 0600 file
		// fallback) so .mcp.json never holds it.
		if err := auth.SaveClientSecret(clientID, clientSecret); err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("Failed to store client secret: %v", err)})
			return
		}

		config, err := saveMCPConfig(oauthEnv(clientID))
		if err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("Failed to save config: %v", err)})
			return
		}

		// Validate the token to get user info; the config is written either way.
		resp := apiResponse{Config: config}
		if token, err := provider.Token(); err == nil {
			resp.DisplayName, resp.Email, _ = validateToken(token)
		}
		writeJSON(w, resp)
	}
}

func validateToken(token string) (displayName, email string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://webexapis.com/v1/people/me", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 401 {
		return "", "", fmt.Errorf("invalid or expired token (401)")
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var result struct {
		DisplayName string   `json:"displayName"`
		Emails      []string `json:"emails"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to parse response: %w", err)
	}

	emailStr := ""
	if len(result.Emails) > 0 {
		emailStr = result.Emails[0]
	}
	return result.DisplayName, emailStr, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204 -- url is our own 127.0.0.1 listener address
	case "darwin":
		cmd = exec.Command("open", url) // #nosec G204 -- url is our own 127.0.0.1 listener address
	default:
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- url is our own 127.0.0.1 listener address
	}
	_ = cmd.Run()
}
