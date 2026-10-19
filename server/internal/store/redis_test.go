package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// NewRedisStore 的 error 會被 main 用 slog 印進 log，不能帶出密碼。
// 所有 case 都連到 127.0.0.1:1（沒有服務）或根本 parse 不過，不需要真的 Redis。
func TestNewRedisStoreDoesNotLeakPassword(t *testing.T) {
	leaks := []string{"s3kr1t", "s3k"}

	cases := []struct {
		name string
		raw  string
	}{
		{"# in password", "redis://:s3k#r1t@127.0.0.1:1"},
		{"invalid port", "redis://:s3kr1t@127.0.0.1:port"},
		{"no scheme", "127.0.0.1:1"},
		{"unreachable", "redis://:s3kr1t@127.0.0.1:1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			// When
			_, err := NewRedisStore(ctx, tc.raw)

			// Then
			if err == nil {
				t.Fatalf("NewRedisStore(%q) error = nil, want error", tc.raw)
			}
			for _, s := range leaks {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error leaks password (contains %q): %v", s, err)
				}
			}
		})
	}
}
