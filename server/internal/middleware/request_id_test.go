package middleware

import (
	"strings"
	"testing"
)

func TestIsValidRequestID(t *testing.T) {
	cases := map[string]bool{
		"abc-123":               true,
		"":                      false,
		"bad id!":               false,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
	}
	for input, want := range cases {
		if got := isValidRequestID(input); got != want {
			t.Errorf("isValidRequestID(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestGenerateValidID(t *testing.T) {
	for range 100 {
		id := generateRequestID()
		if !isValidRequestID(id) {
			t.Errorf("not generating valid id(%q)", id)
		}
	}
}
