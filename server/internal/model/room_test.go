package model

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidPublicID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"generated id", base64.RawURLEncoding.EncodeToString(make([]byte, PublicIDBytes)), true},
		{"legacy id", LegacyRoomPublicID, true},
		{"empty", "", false},
		{"10 chars", strings.Repeat("a", 10), false},
		{"12 chars", strings.Repeat("a", 12), false},
		{"contains =", "abcdefghij=", false},
		{"contains +", "abcdefghij+", false},
		{"contains /", "abcdefghij/", false},
		{"contains space", "abcdefghij ", false},
		{"11 bytes with non-ascii", "abcdefgh中", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidPublicID(tt.in); got != tt.want {
				t.Errorf("ValidPublicID(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
