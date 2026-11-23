package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/prometheus/promtest"
)

// ResyncPublisher test 專用的假 publisher：記下每次 (room, msg)，failFn 決定這次呼叫要不要失敗
// 不用 fakeRedis：它的 publishErr 是整體開關，沒辦法「只讓第二次呼叫（resync）失敗」
type scriptedPublisher struct {
	mu     sync.Mutex
	calls  []publishCall
	failFn func(roomID, msg string) error
}

type publishCall struct{ room, msg string }

func (s *scriptedPublisher) Publish(ctx context.Context, roomID, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, publishCall{roomID, msg})
	if s.failFn != nil {
		return s.failFn(roomID, msg)
	}
	return nil
}

func (s *scriptedPublisher) setFail(fn func(roomID, msg string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFn = fn
}

func (s *scriptedPublisher) snapshot() []publishCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]publishCall(nil), s.calls...)
}

func (s *scriptedPublisher) resyncCount(room string) int {
	n := 0
	for _, c := range s.snapshot() {
		if c.room == room && c.msg == string(encodeResync(room)) {
			n++
		}
	}
	return n
}

var errBoom = errors.New("boom")

func failAll(string, string) error { return errBoom }

func TestResyncPublisherFailThenSuccessSendsOneResyncAfterMessage(t *testing.T) {
	inner := &scriptedPublisher{failFn: failAll}
	p := NewResyncPublisher(inner)
	ctx := context.Background()

	if err := p.Publish(ctx, "r1", "m1"); !errors.Is(err, errBoom) {
		t.Fatalf("Publish err = %v, want errBoom", err)
	}
	inner.setFail(nil)
	if err := p.Publish(ctx, "r1", "m2"); err != nil {
		t.Fatalf("Publish err = %v, want nil", err)
	}

	calls := inner.snapshot()
	want := []publishCall{{"r1", "m1"}, {"r1", "m2"}, {"r1", string(encodeResync("r1"))}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls[%d] = %v, want %v", i, calls[i], want[i])
		}
	}

	// 已經補發過，再發一次不會再有 resync
	if err := p.Publish(ctx, "r1", "m3"); err != nil {
		t.Fatal(err)
	}
	if got := inner.resyncCount("r1"); got != 1 {
		t.Fatalf("resync count = %d, want 1", got)
	}
}

func TestResyncPublisherNeverFailedNoResync(t *testing.T) {
	inner := &scriptedPublisher{}
	p := NewResyncPublisher(inner)
	for i := 0; i < 3; i++ {
		if err := p.Publish(context.Background(), "r1", "m"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(inner.snapshot()); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
	if got := inner.resyncCount("r1"); got != 0 {
		t.Fatalf("resync count = %d, want 0", got)
	}
}

func TestResyncPublisherConcurrentSuccessSendsExactlyOneResync(t *testing.T) {
	inner := &scriptedPublisher{failFn: failAll}
	p := NewResyncPublisher(inner)
	_ = p.Publish(context.Background(), "r1", "m1")
	inner.setFail(nil)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Publish(context.Background(), "r1", "m"); err != nil {
				t.Errorf("Publish err = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := inner.resyncCount("r1"); got != 1 {
		t.Fatalf("resync count = %d, want 1", got)
	}
}

func TestResyncPublisherResyncFailurePutsFlagBack(t *testing.T) {
	inner := &scriptedPublisher{failFn: failAll}
	p := NewResyncPublisher(inner)
	_ = p.Publish(context.Background(), "r1", "m1")

	// 訊息成功、resync frame 失敗
	resync := string(encodeResync("r1"))
	inner.setFail(func(_, msg string) error {
		if msg == resync {
			return errBoom
		}
		return nil
	})
	resyncBefore := promtest.DeferredResyncPublishedCount()
	failedBefore := promtest.PublishFailedCount()
	if err := p.Publish(context.Background(), "r1", "m2"); err != nil {
		t.Fatalf("Publish err = %v, want nil even when resync fails", err)
	}
	p.mu.Lock()
	_, ok := p.pending["r1"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("pending flag should be put back after resync failure")
	}
	// resync 失敗不計 deferred_resync_published，也不計 publish_failed
	if got := promtest.DeferredResyncPublishedCount(); got != resyncBefore {
		t.Fatalf("deferred resync counter = %v, want unchanged %v", got, resyncBefore)
	}
	if got := promtest.PublishFailedCount(); got != failedBefore {
		t.Fatalf("publish_failed counter = %v, want unchanged %v", got, failedBefore)
	}

	inner.setFail(nil)
	before := promtest.DeferredResyncPublishedCount()
	if err := p.Publish(context.Background(), "r1", "m3"); err != nil {
		t.Fatal(err)
	}
	if got := promtest.DeferredResyncPublishedCount(); got != before+1 {
		t.Fatalf("counter = %v, want %v", got, before+1)
	}
	// 失敗的那次也算一次 resync 呼叫，成功的那次是第二次
	if got := inner.resyncCount("r1"); got != 2 {
		t.Fatalf("resync calls = %d, want 2 (one failed, one ok)", got)
	}
}

func TestResyncPublisherPutBackKeepsEarlierTime(t *testing.T) {
	p := NewResyncPublisher(&scriptedPublisher{})
	earlier := time.Now().Add(-time.Hour)
	later := time.Now()

	// 空窗期內別人記了較晚的失敗時間：放回去要換成較早的
	p.pending["r1"] = later
	p.putBack("r1", earlier)
	if got := p.pending["r1"]; !got.Equal(earlier) {
		t.Fatalf("pending = %v, want earlier %v", got, earlier)
	}
	// 反過來：已有較早的，不被較晚的覆蓋
	p.putBack("r1", later)
	if got := p.pending["r1"]; !got.Equal(earlier) {
		t.Fatalf("pending = %v, want earlier %v", got, earlier)
	}
	// 沒有 entry：直接放回
	p.putBack("r2", later)
	if got, ok := p.pending["r2"]; !ok || !got.Equal(later) {
		t.Fatalf("pending[r2] = %v, %v", got, ok)
	}
}

func TestResyncPublisherRoomsAreIndependent(t *testing.T) {
	inner := &scriptedPublisher{failFn: func(room, _ string) error {
		if room == "A" {
			return errBoom
		}
		return nil
	}}
	p := NewResyncPublisher(inner)
	_ = p.Publish(context.Background(), "A", "m1")
	if err := p.Publish(context.Background(), "B", "m2"); err != nil {
		t.Fatal(err)
	}
	if got := inner.resyncCount("B"); got != 0 {
		t.Fatalf("room B resync count = %d, want 0", got)
	}
	if got := inner.resyncCount("A"); got != 0 {
		t.Fatalf("room A resync count = %d, want 0 (A still failing)", got)
	}
	// B 成功不應動到 A 的 pending
	p.mu.Lock()
	_, aPending := p.pending["A"]
	_, bPending := p.pending["B"]
	p.mu.Unlock()
	if !aPending {
		t.Fatal("room A should still be pending after B publishes")
	}
	if bPending {
		t.Fatal("room B should not be pending")
	}

	// A 恢復後：只在 A 補發一次 resync，B 不受影響
	inner.setFail(nil)
	if err := p.Publish(context.Background(), "A", "m3"); err != nil {
		t.Fatal(err)
	}
	if got := inner.resyncCount("A"); got != 1 {
		t.Fatalf("room A resync count = %d, want 1", got)
	}
	if got := inner.resyncCount("B"); got != 0 {
		t.Fatalf("room B resync count = %d, want 0", got)
	}
}

func TestResyncPublisherCounterIncrementsOncePerResync(t *testing.T) {
	inner := &scriptedPublisher{failFn: failAll}
	p := NewResyncPublisher(inner)
	_ = p.Publish(context.Background(), "r1", "m1")
	inner.setFail(nil)

	before := promtest.DeferredResyncPublishedCount()
	_ = p.Publish(context.Background(), "r1", "m2")
	_ = p.Publish(context.Background(), "r1", "m3")
	if got := promtest.DeferredResyncPublishedCount(); got != before+1 {
		t.Fatalf("counter = %v, want %v", got, before+1)
	}
}
