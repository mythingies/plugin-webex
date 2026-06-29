package setup

import (
	"errors"
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

func TestEvalExistingAuth(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	okValidate := func(string) (string, string, error) { return "Ada Lovelace", "ada@example.com", nil }
	failValidate := func(string) (string, string, error) { return "", "", errors.New("401") }

	tests := []struct {
		name       string
		tokens     *auth.StoredTokens
		validate   func(string) (string, string, error)
		wantNil    bool
		wantName   string
		wantAuthed bool // Authenticated() == identity confirmed live
	}{
		{
			name: "valid access token confirms identity",
			tokens: &auth.StoredTokens{
				AccessToken:           "tok",
				AccessTokenExpiresAt:  future,
				RefreshTokenExpiresAt: future,
			},
			validate:   okValidate,
			wantName:   "Ada Lovelace",
			wantAuthed: true,
		},
		{
			name: "expired access but valid refresh -> session exists, no identity",
			tokens: &auth.StoredTokens{
				AccessToken:           "tok",
				AccessTokenExpiresAt:  past,
				RefreshTokenExpiresAt: future,
			},
			validate:   okValidate, // not called: access already expired
			wantAuthed: false,
		},
		{
			name: "access valid but validate fails, refresh valid -> falls back to no-identity session",
			tokens: &auth.StoredTokens{
				AccessToken:           "tok",
				AccessTokenExpiresAt:  future,
				RefreshTokenExpiresAt: future,
			},
			validate:   failValidate,
			wantAuthed: false,
		},
		{
			name: "expired refresh -> nil (must re-auth)",
			tokens: &auth.StoredTokens{
				AccessToken:           "tok",
				AccessTokenExpiresAt:  past,
				RefreshTokenExpiresAt: past,
			},
			validate: okValidate,
			wantNil:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalExistingAuth(tt.tokens, now, tt.validate)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil ExistingAuth, got nil")
			}
			if tt.wantName != "" && got.DisplayName != tt.wantName {
				t.Errorf("DisplayName = %q, want %q", got.DisplayName, tt.wantName)
			}
			if got.Authenticated() != tt.wantAuthed {
				t.Errorf("Authenticated() = %v, want %v", got.Authenticated(), tt.wantAuthed)
			}
		})
	}
}
