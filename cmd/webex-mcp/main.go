package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
	"github.com/mythingies/plugin-webex/internal/server"
	"github.com/mythingies/plugin-webex/internal/setup"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func main() {
	// Handle special subcommands before normal startup.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--oauth-callback":
			handleOAuthCallback()
			return
		case "--register-protocol":
			handleRegisterProtocol()
			return
		case "--setup":
			handleSetup()
			return
		}
	}

	provider := resolveAuth()

	configPath := os.Getenv("WEBEX_AGENTS_CONFIG")
	if configPath == "" {
		configPath = ".webex-agents.yml"
	}

	srv, err := server.New(provider, configPath)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	fmt.Fprintln(os.Stderr, "webex-mcp server starting")
	if err := srv.Start(context.Background()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// handleOAuthCallback is invoked by the OS when a wmcp:// URL is opened.
// It writes the auth code to a file that the waiting Authorize() call reads.
func handleOAuthCallback() {
	if len(os.Args) < 3 {
		log.Fatal("usage: webex-mcp --oauth-callback <wmcp://...>")
	}

	callbackURL := os.Args[2]
	if err := auth.HandleCallbackURL(callbackURL); err != nil {
		log.Fatalf("OAuth callback failed: %v", err)
	}

	fmt.Fprintln(os.Stderr, "Authorization callback received. You can close this window.")
}

// handleSetup launches the browser-based setup UI.
func handleSetup() {
	exe, _ := os.Executable()
	if err := setup.Run(exe); err != nil {
		log.Fatalf("setup error: %v", err)
	}
}

// handleRegisterProtocol registers the wmcp:// custom URI scheme with the OS.
func handleRegisterProtocol() {
	if err := auth.RegisterProtocol(""); err != nil {
		log.Fatalf("failed to register protocol: %v", err)
	}
	fmt.Fprintln(os.Stderr, "Registered wmcp:// protocol handler successfully.")
}

// resolveAuth determines the authentication mode from environment variables.
//
// Priority:
//  1. WEBEX_TOKEN — Personal Access Token (static)
//  2. WEBEX_CLIENT_ID + WEBEX_CLIENT_SECRET — OAuth integration
func resolveAuth() webex.TokenProvider {
	token := strings.TrimSpace(os.Getenv("WEBEX_TOKEN"))
	clientID := strings.TrimSpace(os.Getenv("WEBEX_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("WEBEX_CLIENT_SECRET"))

	switch {
	case token != "":
		fmt.Fprintln(os.Stderr, "auth: using Personal Access Token")
		return auth.NewStaticProvider(token)

	case clientID != "" && clientSecret != "":
		return resolveOAuth(clientID, clientSecret)

	case clientID != "" || clientSecret != "":
		log.Fatal("OAuth credentials incomplete. Both WEBEX_CLIENT_ID and WEBEX_CLIENT_SECRET are required.")

	default:
		log.Fatal("Authentication required.\n\n" +
			"  Option 1 — Personal Access Token (quick start, expires in 12h):\n" +
			"    export WEBEX_TOKEN=<your-token>\n" +
			"    Generate at: https://developer.webex.com/docs/getting-your-personal-access-token\n\n" +
			"  Option 2 — OAuth Integration (persistent, auto-refresh):\n" +
			"    1. Create integration at: https://developer.webex.com/my-apps/new/integration\n" +
			"    2. Set Redirect URI to: wmcp://oauth-callback\n" +
			"    3. Select scopes: spark:messages_read, spark:messages_write, spark:rooms_read,\n" +
			"       spark:memberships_read, spark:people_read, meeting:schedules_read,\n" +
			"       meeting:transcripts_read, meeting:participants_read\n" +
			"       (or spark:all for full access)\n" +
			"    4. Export credentials:\n" +
			"       export WEBEX_CLIENT_ID=<client-id>\n" +
			"       export WEBEX_CLIENT_SECRET=<client-secret>\n" +
			"    5. Register protocol handler: webex-mcp --register-protocol\n")
	}

	return nil // unreachable
}

func resolveOAuth(clientID, clientSecret string) webex.TokenProvider {
	cfg := auth.OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  os.Getenv("WEBEX_REDIRECT_URI"),
		Scopes:       os.Getenv("WEBEX_SCOPES"),
	}

	provider, err := auth.NewOAuthProvider(cfg)
	if err != nil {
		log.Fatalf("failed to create OAuth provider: %v", err)
	}

	if provider.NeedsAuth() {
		fmt.Fprintln(os.Stderr, "auth: starting OAuth authorization flow...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if err := provider.Authorize(ctx); err != nil {
			log.Fatalf("OAuth authorization failed: %v", err)
		}
		fmt.Fprintln(os.Stderr, "auth: authorization successful")
	} else {
		fmt.Fprintln(os.Stderr, "auth: using stored OAuth tokens")
	}

	return provider
}
