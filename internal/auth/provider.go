package auth

import (
	"fmt"
	"strings"
)

// StaticProvider wraps a fixed Personal Access Token.
type StaticProvider struct {
	token string
}

// NewStaticProvider creates a provider that always returns the same token.
func NewStaticProvider(token string) *StaticProvider {
	return &StaticProvider{token: strings.TrimSpace(token)}
}

// Token returns the static access token.
func (p *StaticProvider) Token() (string, error) {
	if p.token == "" {
		return "", fmt.Errorf("static token is empty")
	}
	return p.token, nil
}
