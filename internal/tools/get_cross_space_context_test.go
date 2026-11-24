package tools

import (
	"testing"
)

func TestPluralize(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "es"},
		{1, ""},
		{2, "es"},
		{10, "es"},
	}

	for _, tt := range tests {
		got := pluralize(tt.n)
		if got != tt.want {
			t.Errorf("pluralize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
