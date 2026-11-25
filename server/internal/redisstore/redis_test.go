package redisstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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

// applyClientDefaults 是純函式：零值補我們的預設，已設的（含負值）不動
func TestApplyClientDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   redis.Options
		want redis.Options
	}{
		{
			"all zero gets our defaults",
			redis.Options{},
			redis.Options{ReadTimeout: defaultReadTimeout, DialTimeout: defaultDialTimeout, DialerRetries: defaultDialerRetries},
		},
		{
			"already set values are kept",
			redis.Options{ReadTimeout: 3 * time.Second, DialTimeout: 2 * time.Second, DialerRetries: 4},
			redis.Options{ReadTimeout: 3 * time.Second, DialTimeout: 2 * time.Second, DialerRetries: 4},
		},
		{
			"negative read timeout (no timeout) is kept",
			redis.Options{ReadTimeout: -1},
			redis.Options{ReadTimeout: -1, DialTimeout: defaultDialTimeout, DialerRetries: defaultDialerRetries},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := tc.in
			applyClientDefaults(&opt)
			if opt.ReadTimeout != tc.want.ReadTimeout || opt.DialTimeout != tc.want.DialTimeout || opt.DialerRetries != tc.want.DialerRetries {
				t.Errorf("got read=%v dial=%v retries=%d, want read=%v dial=%v retries=%d",
					opt.ReadTimeout, opt.DialTimeout, opt.DialerRetries,
					tc.want.ReadTimeout, tc.want.DialTimeout, tc.want.DialerRetries)
			}
		})
	}
}

// URL 沒帶參數用我們的預設；帶 ?read_timeout=3s 的以 URL 為準（prod 可覆蓋）
func TestApplyClientDefaultsWithParseURL(t *testing.T) {
	t.Run("plain url gets our defaults", func(t *testing.T) {
		opt, err := redis.ParseURL("redis://localhost:6379/0")
		if err != nil {
			t.Fatal(err)
		}
		applyClientDefaults(opt)
		if opt.ReadTimeout != defaultReadTimeout || opt.DialTimeout != defaultDialTimeout || opt.DialerRetries != defaultDialerRetries {
			t.Errorf("got read=%v dial=%v retries=%d", opt.ReadTimeout, opt.DialTimeout, opt.DialerRetries)
		}
	})
	t.Run("url read_timeout wins", func(t *testing.T) {
		opt, err := redis.ParseURL("redis://localhost:6379/0?read_timeout=3s")
		if err != nil {
			t.Fatal(err)
		}
		applyClientDefaults(opt)
		if opt.ReadTimeout != 3*time.Second {
			t.Errorf("ReadTimeout = %v, want 3s", opt.ReadTimeout)
		}
		if opt.DialTimeout != defaultDialTimeout {
			t.Errorf("DialTimeout = %v, want default", opt.DialTimeout)
		}
	})
}
