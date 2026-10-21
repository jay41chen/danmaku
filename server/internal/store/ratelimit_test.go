package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// 需要真的 Redis：沒設 TEST_REDIS_URL 就 skip（見 newTestRateLimiter）
func TestAllow(t *testing.T) {
	t.Run("allows up to limit then rejects with remaining window", func(t *testing.T) {
		s, key := newTestRateLimiter(t)
		limit, window := 3, time.Minute

		for i := 1; i <= limit; i++ {
			allowed, _, err := s.Allow(t.Context(), key, limit, window)
			if err != nil || !allowed {
				t.Fatalf("Allow #%d = (%v, %v), want (true, nil)", i, allowed, err)
			}
		}

		allowed, retryAfter, err := s.Allow(t.Context(), key, limit, window)

		if err != nil || allowed {
			t.Fatalf("Allow #%d = (%v, %v), want (false, nil)", limit+1, allowed, err)
		}
		if retryAfter <= 0 || retryAfter > window {
			t.Errorf("retryAfter = %v, want in (0, %v]", retryAfter, window)
		}
	})

	// zombie key：INCR 了但沒設過期時間，會被永久封鎖
	t.Run("first request sets expiry", func(t *testing.T) {
		s, key := newTestRateLimiter(t)

		if _, _, err := s.Allow(t.Context(), key, 3, time.Minute); err != nil {
			t.Fatalf("Allow error = %v", err)
		}

		ttl, err := s.client.PTTL(t.Context(), key).Result()
		if err != nil {
			t.Fatalf("PTTL error = %v", err)
		}
		// -1 = 沒有過期時間（zombie），-2 = key 不存在
		if ttl <= 0 {
			t.Errorf("PTTL = %v, want > 0", ttl)
		}
	})

	// 第二次之後的 INCR 不能延長窗口，否則一直打就永遠解不開
	t.Run("later requests do not extend window", func(t *testing.T) {
		s, key := newTestRateLimiter(t)
		window := 300 * time.Millisecond

		s.Allow(t.Context(), key, 1, window)
		time.Sleep(200 * time.Millisecond)
		_, retryAfter, err := s.Allow(t.Context(), key, 1, window)

		if err != nil {
			t.Fatalf("Allow error = %v", err)
		}
		// 窗口從第一次開始算，過了 200ms 只剩約 100ms；重設的話會接近 300ms
		if retryAfter > 150*time.Millisecond {
			t.Errorf("retryAfter = %v, want <= 150ms (window must not restart)", retryAfter)
		}
	})

	t.Run("allows again after window expires", func(t *testing.T) {
		s, key := newTestRateLimiter(t)
		window := 100 * time.Millisecond

		s.Allow(t.Context(), key, 1, window)
		if allowed, _, _ := s.Allow(t.Context(), key, 1, window); allowed {
			t.Fatalf("second Allow within window = true, want false")
		}
		time.Sleep(150 * time.Millisecond)

		allowed, _, err := s.Allow(t.Context(), key, 1, window)

		if err != nil || !allowed {
			t.Errorf("Allow after window = (%v, %v), want (true, nil)", allowed, err)
		}
	})
}

func TestReset(t *testing.T) {
	t.Run("clears count", func(t *testing.T) {
		s, key := newTestRateLimiter(t)
		s.Allow(t.Context(), key, 1, time.Minute)

		if err := s.Reset(t.Context(), key); err != nil {
			t.Fatalf("Reset error = %v", err)
		}

		allowed, _, err := s.Allow(t.Context(), key, 1, time.Minute)
		if err != nil || !allowed {
			t.Errorf("Allow after Reset = (%v, %v), want (true, nil)", allowed, err)
		}
	})

	t.Run("missing key is not an error", func(t *testing.T) {
		s, key := newTestRateLimiter(t)

		if err := s.Reset(t.Context(), key); err != nil {
			t.Errorf("Reset(missing) error = %v, want nil", err)
		}
	})
}

// 每個 test 用自己的 key（rl:test:<test 名稱>），不 FLUSHDB：本機 Redis 可能有其他資料
func newTestRateLimiter(t *testing.T) (*RedisStore, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}

	s, err := NewRedisStore(t.Context(), url)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(func() { s.client.Close() })

	key := "rl:test:" + t.Name()
	// 上次跑到一半中斷可能留下舊的 key，先清掉；結束時也清
	// Cleanup 時 t.Context() 已經被取消，所以用 Background
	s.client.Del(t.Context(), key)
	t.Cleanup(func() { s.client.Del(context.Background(), key) })
	return s, key
}

func TestRefund(t *testing.T) {
	t.Run("gives back one request", func(t *testing.T) {
		s, key := newTestRateLimiter(t)
		s.Allow(t.Context(), key, 1, time.Minute)

		if err := s.Refund(t.Context(), key); err != nil {
			t.Fatalf("Refund error = %v", err)
		}

		// 上限 1、已經用掉 1 次：沒退還的話這次會被擋
		allowed, _, err := s.Allow(t.Context(), key, 1, time.Minute)
		if err != nil || !allowed {
			t.Errorf("Allow after Refund = (%v, %v), want (true, nil)", allowed, err)
		}
	})

	// window 在 Allow 和 Refund 之間過期：直接 DECR 會建出 -1 而且沒有 TTL 的 key
	t.Run("expired key is not recreated", func(t *testing.T) {
		s, key := newTestRateLimiter(t)

		if err := s.Refund(t.Context(), key); err != nil {
			t.Fatalf("Refund error = %v", err)
		}

		n, err := s.client.Exists(t.Context(), key).Result()
		if err != nil {
			t.Fatalf("EXISTS error = %v", err)
		}
		if n != 0 {
			val, _ := s.client.Get(t.Context(), key).Result()
			t.Errorf("key exists after Refund on missing key (value %q), want not exist", val)
		}
	})
}
