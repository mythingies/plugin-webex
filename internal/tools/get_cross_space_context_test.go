package tools

import (
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is a longer string", 10, "this is a ..."},
		{"", 5, ""},
	}

	for _, tt := range tests {
		got := truncate(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

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
