package setup

import (
	"errors"
	"os"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

// ExistingAuth describes a currently-stored OAuth session.
type ExistingAuth struct {
	DisplayName    string // from people/me; empty if the access token couldn't be validated live
	Email          string
	AccessExpires  time.Time
	RefreshExpires time.Time
}

// Authenticated reports whether the identity was confirmed against the Webex
// API (people/me) on this call, versus inferred from a still-valid refresh
// token without a live check.
func (e *ExistingAuth) Authenticated() bool { return e.DisplayName != "" || e.Email != "" }

// CurrentAuth loads stored OAuth tokens and reports the existing session, if
// any. It returns (nil, nil) when there is nothing usable — no tokens, or the
// refresh token has expired — so the caller falls through to the interactive
// setup flow. A non-nil result means the user is already signed in and setup
// can be skipped (unless they explicitly want to switch or force re-auth).
//
// It never errors out the caller: any inability to determine state is treated
// as "no existing session" so --setup still works.
func CurrentAuth() *ExistingAuth {
	store, err := auth.NewTokenStore()
	if err != nil {
		return nil
	}
	tokens, err := store.Load()
	if err != nil || tokens == nil {
		return nil
	}
	return evalExistingAuth(tokens, time.Now(), validateToken)
}

// evalExistingAuth holds the pure session-state decision logic, separated from
// keychain/HTTP I/O so it can be unit-tested. validate is the identity check
// (live people/me call in production); now is the reference time.
func evalExistingAuth(tokens *auth.StoredTokens, now time.Time, validate func(string) (string, string, error)) *ExistingAuth {
	// If the refresh token has expired, the session is dead — must re-auth.
	if !tokens.RefreshTokenExpiresAt.IsZero() && now.After(tokens.RefreshTokenExpiresAt) {
		return nil
	}

	// Try to confirm identity with the stored access token. A successful call
	// gives us the display name/email to show the user.
	if tokens.AccessToken != "" && now.Before(tokens.AccessTokenExpiresAt) {
		if name, email, err := validate(tokens.AccessToken); err == nil {
			return &ExistingAuth{
				DisplayName:    name,
				Email:          email,
				AccessExpires:  tokens.AccessTokenExpiresAt,
				RefreshExpires: tokens.RefreshTokenExpiresAt,
			}
		}
	}

	// Access token expired or unverifiable, but the refresh token is still
	// valid — the server will silently refresh on next launch. Report the
	// session as existing, without a confirmed identity.
	if !tokens.RefreshTokenExpiresAt.IsZero() && now.Before(tokens.RefreshTokenExpiresAt) {
		return &ExistingAuth{
			AccessExpires:  tokens.AccessTokenExpiresAt,
			RefreshExpires: tokens.RefreshTokenExpiresAt,
		}
	}

	return nil
}

// Logout removes stored OAuth tokens so the next setup run authenticates from
// scratch (used by --switch and --logout). The OAuth client secret is left in
// place — switching Webex user under the same integration only needs the
// tokens cleared. A missing token entry is not an error.
func Logout() error {
	store, err := auth.NewTokenStore()
	if err != nil {
		return err
	}
	if err := store.Delete(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
