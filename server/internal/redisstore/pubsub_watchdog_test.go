package redisstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// 不需要 Redis：publish / rebuild 都是假的，tick 由 test 手動送
// 同步方式：tick 是無緩衝 channel，publish 是每輪的最後一步，publish 的 fake 做完回報 publishDone →
// round() 回來時這一輪（含 rebuild）一定做完了，不用 sleep
// run 沒有收 tick 時，round() 會在 roundTimeout 後 fail，而不是卡死
type watchdogHarness struct {
	t           *testing.T
	w           *pubsubWatchdog
	tick        chan time.Time
	publishDone chan struct{}
	publishes   atomic.Int32
	rebuilds    atomic.Int32
	failNext    atomic.Bool // true 時下一次 publish 回 error（用完自動清掉）
	cancel      context.CancelFunc
	done        chan struct{} // run return 後 close
}

const roundTimeout = time.Second

var errFakePublish = errors.New("fake publish failed")

func newWatchdogHarness(t *testing.T) *watchdogHarness {
	h := &watchdogHarness{
		t:           t,
		tick:        make(chan time.Time),
		publishDone: make(chan struct{}),
		done:        make(chan struct{}),
	}
	h.w = newPubsubWatchdog(
		func(ctx context.Context) error {
			h.publishes.Add(1)
			var err error
			if h.failNext.CompareAndSwap(true, false) {
				err = errFakePublish
			}
			// run 的 ctx 被取消時沒人收 publishDone：用 select 避免 goroutine 卡住
			select {
			case h.publishDone <- struct{}{}:
			case <-time.After(roundTimeout):
			}
			return err
		},
		func(ctx context.Context) { h.rebuilds.Add(1) },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.w.run(ctx, h.tick)
		close(h.done)
	}()
	t.Cleanup(cancel)
	return h
}

// 送一個 tick，等到這一輪做完（publish 被呼叫）
func (h *watchdogHarness) round() {
	h.t.Helper()
	select {
	case h.tick <- time.Now():
	case <-time.After(roundTimeout):
		h.t.Fatal("run did not accept a tick")
	}
	select {
	case <-h.publishDone:
	case <-time.After(roundTimeout):
		h.t.Fatal("run did not publish after tick")
	}
}

func (h *watchdogHarness) wantRebuilds(want int32) {
	h.t.Helper()
	if got := h.rebuilds.Load(); got != want {
		h.t.Fatalf("rebuilds = %d, want %d", got, want)
	}
}

func TestWatchdogNeverRebuildsWhileHeartbeatsArrive(t *testing.T) {
	h := newWatchdogHarness(t)
	for range 6 {
		h.w.beat()
		h.round()
	}
	h.wantRebuilds(0)
}

// 第一次 tick 之前沒有 publish 過，沒收到心跳是正常的，不能算 miss
func TestWatchdogFirstTickIsNotAMiss(t *testing.T) {
	h := newWatchdogHarness(t)
	h.round() // 還沒 publish 過，不算
	h.round() // miss 1
	h.wantRebuilds(0)
	h.round() // miss 2 → rebuild
	h.wantRebuilds(1)
}

func TestWatchdogRebuildsOnlyAfterConsecutiveMisses(t *testing.T) {
	h := newWatchdogHarness(t)
	h.round() // 第一輪：只 publish
	h.round() // miss 1
	h.wantRebuilds(0)
	h.round() // miss 2 → rebuild
	h.wantRebuilds(1)
	h.round() // rebuild 之後 misses 歸零，這輪是 miss 1
	h.wantRebuilds(1)
}

func TestWatchdogMissesResetAfterRebuild(t *testing.T) {
	h := newWatchdogHarness(t)
	h.round()
	h.round()
	h.round() // 第一次 rebuild
	h.wantRebuilds(1)
	h.round() // miss 1
	h.wantRebuilds(1)
	h.round() // miss 2 → 第二次 rebuild
	h.wantRebuilds(2)
}

// publish 失敗的那一輪發不出心跳，下一輪沒收到不是 PubSub 的錯
func TestWatchdogMissAfterFailedPublishIsNotCounted(t *testing.T) {
	h := newWatchdogHarness(t)
	h.failNext.Store(true)
	h.round() // publish 失敗
	h.round() // 沒收到，但上一次 publish 失敗 → 不算；這輪 publish 成功
	h.round() // miss 1
	h.wantRebuilds(0)
	h.round() // miss 2 → rebuild
	h.wantRebuilds(1)
}

func TestWatchdogReceivedHeartbeatResetsMisses(t *testing.T) {
	h := newWatchdogHarness(t)
	h.round()
	h.round() // miss 1
	h.w.beat()
	h.round() // 收到 → 歸零
	h.round() // miss 1（不是 2）
	h.wantRebuilds(0)
}

func TestWatchdogRunReturnsWhenContextCanceled(t *testing.T) {
	h := newWatchdogHarness(t)
	h.round()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(roundTimeout):
		t.Fatal("run did not return after ctx canceled")
	}
}

func TestWatchdogPublishesOnEveryTick(t *testing.T) {
	h := newWatchdogHarness(t)
	for i := 1; i <= 4; i++ {
		h.round()
		if got := h.publishes.Load(); got != int32(i) {
			t.Fatalf("publishes after %d ticks = %d, want %d", i, got, i)
		}
	}
}

// beat 不能阻塞：buffer 只有 1，連呼叫兩次（中間沒人取）第二次要直接丟掉，不然 Subscribe 的消費迴圈會卡住
func TestWatchdogBeatTwiceDoesNotBlock(t *testing.T) {
	w := newPubsubWatchdog(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		w.beat()
		w.beat()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(roundTimeout):
		t.Fatal("second beat blocked")
	}
	if got := len(w.received); got != 1 {
		t.Errorf("buffered signals = %d, want 1", got)
	}
}
