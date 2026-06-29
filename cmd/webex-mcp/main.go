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
	"github.com/mythingies/plugin-webex/internal/tools"
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
			handleSetup(hasFlag("--force"))
			return
		case "--switch":
			handleSwitch()
			return
		case "--logout":
			handleLogout()
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

// hasFlag reports whether the given flag appears anywhere in os.Args[2:].
func hasFlag(flag string) bool {
	for _, a := range os.Args[2:] {
		if a == flag {
			return true
		}
	}
	return false
}

// handleSetup launches the browser-based setup UI, but first checks whether a
// usable OAuth session already exists. If so (and --force was not passed), it
// tells the user they're already signed in and exits without opening a browser
// — re-authenticating only on explicit request. This avoids the confusing case
// where --setup pushes the user through a redundant (and sometimes stalling)
// browser OAuth flow even though their stored tokens are already valid.
func handleSetup(force bool) {
	if !force {
		if existing := setup.CurrentAuth(); existing != nil {
			printAlreadyAuthed(existing)
			return
		}
	}
	runSetup()
}

// runSetup launches the interactive browser setup UI.
func runSetup() {
	exe, _ := os.Executable()
	if err := setup.Run(exe); err != nil {
		log.Fatalf("setup error: %v", err)
	}
}

// handleSwitch signs out the current Webex user and starts a fresh setup flow
// so a different account can authenticate — the gh-CLI "auth switch" analogue.
func handleSwitch() {
	if err := setup.Logout(); err != nil {
		log.Fatalf("failed to clear current session: %v", err)
	}
	fmt.Fprintln(os.Stderr, "Signed out the current Webex user. Starting setup for a new account...")
	runSetup()
}

// handleLogout removes stored OAuth tokens without starting a new setup flow.
func handleLogout() {
	if err := setup.Logout(); err != nil {
		log.Fatalf("failed to log out: %v", err)
	}
	fmt.Fprintln(os.Stderr, "Logged out. Stored OAuth tokens removed. Run `webex-mcp --setup` to sign in again.")
}

// printAlreadyAuthed reports an existing session and the available next steps,
// without opening a browser.
func printAlreadyAuthed(e *setup.ExistingAuth) {
	if e.Authenticated() {
		who := e.DisplayName
		if e.Email != "" {
			who += " <" + e.Email + ">"
		}
		fmt.Fprintf(os.Stderr, "\n✓ Already authenticated as %s\n", who)
	} else {
		fmt.Fprintf(os.Stderr, "\n✓ Already authenticated (stored session is valid)\n")
	}
	if !e.AccessExpires.IsZero() {
		fmt.Fprintf(os.Stderr, "  Access token valid until %s", e.AccessExpires.Format("2006-01-02 15:04 MST"))
		if !e.RefreshExpires.IsZero() {
			fmt.Fprintf(os.Stderr, "; refresh until %s", e.RefreshExpires.Format("2006-01-02"))
		}
		fmt.Fprintln(os.Stderr)
	}
	fmt.Fprintln(os.Stderr, "\n  Nothing to do — reload Claude Code to start using it.")
	fmt.Fprintln(os.Stderr, "\n  To sign in as a different Webex account:  webex-mcp --switch")
	fmt.Fprintln(os.Stderr, "  To force re-authentication (same account): webex-mcp --setup --force")
	fmt.Fprintln(os.Stderr, "  To sign out without re-authenticating:    webex-mcp --logout")
}

// handleRegisterProtocol registers the wmcp:// custom URI scheme with the OS.
func handleRegisterProtocol() {
	if err := auth.RegisterProtocol(""); err != nil {
		log.Fatalf("failed to register protocol: %v", err)
	}
	fmt.Fprintln(os.Stderr, "Registered wmcp:// protocol handler successfully.")
}

// resolveAuth determines the authentication mode from environment variables
// and the OS keychain.
//
// Priority:
//  1. WEBEX_TOKEN — Personal Access Token (static)
//  2. WEBEX_CLIENT_ID + WEBEX_CLIENT_SECRET (env) — OAuth integration; env
//     wins when present, useful for CI/testing
//  3. WEBEX_CLIENT_ID (env) + secret from OS keychain — preferred path
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

	case clientID != "":
		secret, err := auth.LoadClientSecret(clientID)
		if err != nil {
			log.Fatal("OAuth client secret not found in keychain for the configured WEBEX_CLIENT_ID. Run `webex-mcp --setup` to store it, or set WEBEX_CLIENT_SECRET in the environment.")
		}
		return resolveOAuth(clientID, secret)

	case clientSecret != "":
		log.Fatal("OAuth credentials incomplete. WEBEX_CLIENT_ID is required.")

	default:
		log.Fatal("Authentication required.\n\n" +
			"  Option 1 — Personal Access Token (quick start, expires in 12h):\n" +
			"    export WEBEX_TOKEN=<your-token>\n" +
			"    Generate at: https://developer.webex.com/docs/getting-your-personal-access-token\n\n" +
			"  Option 2 — OAuth Integration (persistent, auto-refresh):\n" +
			"    1. Create integration at: https://developer.webex.com/my-apps/new/integration\n" +
			"    2. Set Redirect URI to: wmcp://oauth-callback\n" +
			"    3. Select scopes: spark:all, meeting:schedules_read, meeting:transcripts_read\n" +
			"       (spark:all is required for the real-time listener — WDM device\n" +
			"        registration rejects granular scopes with HTTP 403)\n" +
			"    4. Export credentials:\n" +
			"       export WEBEX_CLIENT_ID=<client-id>\n" +
			"       export WEBEX_CLIENT_SECRET=<client-secret>\n" +
			"    5. Register protocol handler: webex-mcp --register-protocol\n")
	}

	return nil // unreachable
}

func resolveOAuth(clientID, clientSecret string) webex.TokenProvider {
	scopes := os.Getenv("WEBEX_SCOPES")
	cfg := auth.OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  os.Getenv("WEBEX_REDIRECT_URI"),
		Scopes:       scopes,
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

	// Validate that configured scopes cover tool requirements.
	effectiveScopes := scopes
	if effectiveScopes == "" {
		effectiveScopes = auth.DefaultScopes
	}
	if warnings := tools.ValidateScopes(effectiveScopes); len(warnings) > 0 {
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "scope warning: %s\n", w)
		}
	}

	return provider
}
