package textcut

import (
	"testing"
	"unicode/utf8"
)

func TestPrefix(t *testing.T) {
	tests := []struct {
		name, s string
		n       int
		want    string
	}{
		{"fits", "abc", 3, "abc"},
		{"ascii", "abcdef", 4, "abcd"},
		{"on a boundary", "aé", 3, "aé"},
		{"inside a rune", "aé", 2, "a"},
		{"inside a four-byte rune", "a😀b", 3, "a"},
		{"nothing fits", "😀", 2, ""},
		{"zero", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Prefix(tt.s, tt.n)
			if got != tt.want || !utf8.ValidString(got) {
				t.Fatalf("Prefix(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name, s string
		n       int
		want    string
	}{
		{"fits", "abc", 3, "abc"},
		{"cut", "abcdef", 4, "abcd\n[truncated 2 bytes]"},
		{"inside a rune", "aéb", 2, "a\n[truncated 3 bytes]"},
		{"disabled", "abc", 0, "abc"},
		{"negative", "abc", -1, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truncate(tt.s, tt.n); got != tt.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}
