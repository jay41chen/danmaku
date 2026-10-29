package randx

import (
	"strings"
	"testing"
)

func isURLSafeBase64(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '_'
}

func TestURLText(t *testing.T) {
	// 長度是 nBytes × 4/3 向上取整：8 → 11（room public_id）、32 → 43（refresh token）
	for nBytes, wantLen := range map[int]int{8: 11, 32: 43} {
		got := URLText(nBytes)

		if len(got) != wantLen {
			t.Errorf("len(URLText(%d)) = %d, want %d", nBytes, len(got), wantLen)
		}
		if i := strings.IndexFunc(got, func(r rune) bool { return !isURLSafeBase64(r) }); i >= 0 {
			t.Errorf("URLText(%d) = %q has %q at %d, want only A-Z a-z 0-9 - _", nBytes, got, got[i], i)
		}
	}

	if a, b := URLText(8), URLText(8); a == b {
		t.Errorf("two calls both returned %q, want different", a)
	}
}
