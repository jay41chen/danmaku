package redisstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

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
		s.pubsub.Load().Close()
		s.client.Close()
	})

	// 換成假時鐘：LeaveRoom 之後的 UNSUBSCRIBE 要等 unsubscribeDelay（30 秒），test 手動讓時間到
	clock := &fakeClock{}
	s.rooms.afterFunc = clock.afterFunc

	type received struct{ roomID, msg string }
	got := make(chan received, 10)
	go s.Subscribe(t.Context(), func(roomID, msg string) {
		got <- received{roomID, msg}
	}, func(string) {})

	room := "test-" + t.Name()
	joinCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.JoinRoom(joinCtx, room); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	// JoinRoom 回傳 = Redis 已經確認 SUBSCRIBE：不用等，馬上查就要看到訂閱者
	// （以前 JoinRoom 只寫出指令就回傳，緊接著的 PUBLISH 可能比 SUBSCRIBE 先被處理 → 訊息掉了，test 偶爾 fail）
	// test 環境只有這一個訂閱者，NUMSUB 才能當成「這台訂閱好了」；production 多台 server 時不行
	if n := numSub(t, s, room); n != 1 {
		t.Fatalf("NUMSUB right after JoinRoom = %d, want 1", n)
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
	// expire 只標 dirty，背景 worker 才送 UNSUBSCRIBE，而且只寫出指令、不等確認：等 NUMSUB 變 0 再 PUBLISH，否則 PUBLISH 可能先被處理、還收得到
	clock.fireAll()
	waitNumSub(t, s, room, 0)
	if err := s.Publish(t.Context(), room, "after unsubscribe"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case r := <-got:
		t.Fatalf("received %+v after unsubscribe, want nothing", r)
	case <-time.After(300 * time.Millisecond):
	}
}

func numSub(t *testing.T, s *RedisStore, room string) int64 {
	t.Helper()
	counts, err := s.client.PubSubNumSub(t.Context(), roomChannel(room)).Result()
	if err != nil {
		t.Fatalf("PUBSUB NUMSUB: %v", err)
	}
	return counts[roomChannel(room)]
}

// 輪詢到 NUMSUB == want（等的是「Redis 處理完了」，不是固定時間）
func waitNumSub(t *testing.T, s *RedisStore, room string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := numSub(t, s, room)
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("NUMSUB = %d, want %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 需要真的 Redis：沒設 TEST_REDIS_URL 就 skip
// 保護重建流程的接線（單元測試各自用假的，接線錯了抓不到）：
// watchdog 判定聾掉 → rebuildPubSub 換新 PubSub、Close 舊的 → Subscribe 的 consumer 把舊 channel 收完 → resetAfterRebuild →
// worker 在新連線重送 SUB → 確認回來 → onResync；新 PubSub 也要訂心跳，心跳不能進房間的 handler
//
// 直接呼叫 rebuildPubSub，不等 15 秒的 ticker。NewRedisStore 自己也會起 watchdog goroutine，
// 但它只在 ticker 觸發（15 秒後）才讀 received；這個 test 遠短於 15 秒，所以測試讀 received 不會跟它搶
func TestRebuildPubSubWiring(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}
	s, err := NewRedisStore(t.Context(), url)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(func() {
		s.pubsub.Load().Close()
		s.client.Close()
	})

	type received struct{ roomID, msg string }
	got := make(chan received, 10)
	resynced := make(chan string, 10)
	go s.Subscribe(t.Context(), func(roomID, msg string) {
		got <- received{roomID, msg}
	}, func(roomID string) { resynced <- roomID })

	expectMsg := func(want received) {
		t.Helper()
		select {
		case r := <-got:
			if r != want {
				t.Fatalf("handler received %+v, want %+v", r, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("handler did not receive %+v", want)
		}
	}
	// 發一則心跳，等 consumer 收到後 beat 進 watchdog.received
	expectHeartbeat := func(when string) {
		t.Helper()
		if err := s.publishHeartbeat(t.Context()); err != nil {
			t.Fatalf("publishHeartbeat %s: %v", when, err)
		}
		select {
		case <-s.watchdog.received:
		case <-time.After(2 * time.Second):
			t.Fatalf("watchdog did not receive heartbeat %s", when)
		}
	}

	roomA, roomB := "test-"+t.Name()+"-a", "test-"+t.Name()+"-b"
	join := func(room string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := s.JoinRoom(ctx, room); err != nil {
			t.Fatalf("JoinRoom(%q): %v", room, err)
		}
	}
	join(roomA)
	expectHeartbeat("before rebuild")

	old := s.pubsub.Load()
	s.rebuildPubSub(t.Context())
	if s.pubsub.Load() == old {
		t.Fatal("rebuildPubSub did not replace the PubSub")
	}
	// 舊的已經被 rebuild Close：再 Close 一次要回 ErrClosed
	if err := old.Close(); !errors.Is(err, redis.ErrClosed) {
		t.Errorf("old PubSub Close() = %v, want ErrClosed (rebuild should have closed it)", err)
	}

	// consumer 收完舊 channel → reset → worker 在新連線重送 SUB → 確認 → onResync（只有走完整條才會來）
	select {
	case room := <-resynced:
		if room != roomA {
			t.Fatalf("onResync room = %q, want %q", room, roomA)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onResync was not called after rebuild")
	}

	if err := s.Publish(t.Context(), roomA, "after rebuild"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	expectMsg(received{roomA, "after rebuild"})

	// 重建後第一次 Join 新房：要等確認才回傳（NUMSUB 已經是 1），而且不會 resync
	join(roomB)
	if n := numSub(t, s, roomB); n != 1 {
		t.Fatalf("NUMSUB right after JoinRoom(b) = %d, want 1", n)
	}

	// 新 PubSub 有訂心跳；心跳不會進房間的 handler（後面的房間訊息會是 handler 收到的第一則）
	expectHeartbeat("after rebuild")
	if err := s.Publish(t.Context(), roomB, "room b"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	expectMsg(received{roomB, "room b"})

	// 到這裡 resync 恰好一次（a），b 沒有
	select {
	case room := <-resynced:
		t.Fatalf("unexpected extra onResync for %q", room)
	default:
	}
}
