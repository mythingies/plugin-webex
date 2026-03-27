package setup

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

//go:embed setup.html
var setupHTML embed.FS

const version = "v1.0.0"

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

	switch {
	case clientID != "" && clientSecret != "":
		masked := clientID[:4] + "..." + clientID[len(clientID)-4:]
		writeJSON(w, detectResponse{
			Mode:     "oauth",
			ClientID: masked,
			Message:  "OAuth credentials loaded from environment",
		})
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

	config, err := writeMCPConfig("pat", token, "", "")
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

		// Register protocol handler.
		if binaryPath != "" {
			_ = auth.RegisterProtocol(binaryPath)
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

		if err := provider.Authorize(ctx); err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("Authorization failed: %v", err)})
			return
		}

		// Validate the token to get user info.
		token, err := provider.Token()
		if err == nil {
			name, email, _ := validateToken(token)
			config, err := writeMCPConfig("oauth", "", clientID, clientSecret)
			if err != nil {
				writeJSON(w, apiResponse{Error: fmt.Sprintf("Failed to save config: %v", err)})
				return
			}
			writeJSON(w, apiResponse{
				DisplayName: name,
				Email:       email,
				Config:      config,
			})
			return
		}

		config, err := writeMCPConfig("oauth", "", clientID, clientSecret)
		if err != nil {
			writeJSON(w, apiResponse{Error: fmt.Sprintf("Failed to save config: %v", err)})
			return
		}
		writeJSON(w, apiResponse{Config: config})
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

func writeMCPConfig(mode, token, clientID, clientSecret string) (string, error) {
	exe, _ := os.Executable()
	exePath, _ := filepath.Abs(exe)

	env := map[string]string{}
	switch mode {
	case "pat":
		env["WEBEX_TOKEN"] = token
	case "oauth":
		env["WEBEX_CLIENT_ID"] = clientID
		env["WEBEX_CLIENT_SECRET"] = clientSecret
	}

	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"webex": map[string]interface{}{
				"command": exePath,
				"env":     env,
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(".mcp.json", data, 0600); err != nil {
		return "", err
	}

	// Display version with masked secrets.
	displayEnv := map[string]string{}
	for k, v := range env {
		if len(v) > 8 {
			displayEnv[k] = v[:4] + "..." + v[len(v)-4:]
		} else {
			displayEnv[k] = "****"
		}
	}

	displayConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"webex": map[string]interface{}{
				"command": exePath,
				"env":     displayEnv,
			},
		},
	}
	display, _ := json.MarshalIndent(displayConfig, "", "  ")
	return string(display), nil
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
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) //nolint:gosec // trusted localhost URL
	case "darwin":
		cmd = exec.Command("open", url) //nolint:gosec // trusted localhost URL
	default:
		cmd = exec.Command("xdg-open", url) //nolint:gosec // trusted localhost URL
	}
	_ = cmd.Run()
}
