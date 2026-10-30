package store

import (
	"context"
	"os"
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

// 需要真的 Redis：沒設 TEST_REDIS_URL 就 skip
// 驗證 per-room channel：JoinRoom 之後收得到該房間的訊息，LeaveRoom 之後收不到
func TestRoomPubSub(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}
	s, err := NewRedisStore(t.Context(), url)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	// Close PubSub 會讓 Subscribe 的 Channel() 關閉、goroutine 結束
	t.Cleanup(func() {
		s.pubsub.Close()
		s.client.Close()
	})

	// 換成假時鐘：LeaveRoom 之後的 UNSUBSCRIBE 要等 unsubscribeDelay（30 秒），test 手動讓時間到
	clock := &fakeClock{}
	s.rooms.afterFunc = clock.afterFunc

	type received struct{ roomID, msg string }
	got := make(chan received, 10)
	go s.Subscribe(t.Context(), func(roomID, msg string) {
		got <- received{roomID, msg}
	})

	room := "test-" + t.Name()
	if err := s.JoinRoom(t.Context(), room); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	if err := s.Publish(t.Context(), room, "hello"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case r := <-got:
		if r != (received{room, "hello"}) {
			t.Fatalf("received %+v, want room %q msg %q", r, room, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message after JoinRoom")
	}

	// debounce：LeaveRoom 之後、時間到之前還訂閱著，訊息照收
	s.LeaveRoom(t.Context(), room)
	if err := s.Publish(t.Context(), room, "during delay"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case r := <-got:
		if r != (received{room, "during delay"}) {
			t.Fatalf("received %+v, want room %q msg %q", r, room, "during delay")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message during unsubscribe delay")
	}

	// 時間到：真的 UNSUBSCRIBE，之後收不到
	clock.fireAll()
	if err := s.Publish(t.Context(), room, "after unsubscribe"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case r := <-got:
		t.Fatalf("received %+v after unsubscribe, want nothing", r)
	case <-time.After(300 * time.Millisecond):
	}
}
