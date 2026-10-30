package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
)

// 假的 SUBSCRIBE / UNSUBSCRIBE：依序記下呼叫，例如 []string{"sub room:a", "unsub room:a"}
// failSub / failUnsub = true 時回 error（但一樣記下呼叫）
type fakePubSub struct {
	mu        sync.Mutex // 併發 test 用；正常情況 roomSubscriptions 已經在鎖內呼叫，多一把是保險
	calls     []string
	failSub   bool
	failUnsub bool
}

var (
	errFakeSubscribe   = errors.New("fake subscribe failed")
	errFakeUnsubscribe = errors.New("fake unsubscribe failed")
)

func (this *fakePubSub) subscribe(ctx context.Context, channel string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	this.calls = append(this.calls, "sub "+channel)
	if this.failSub {
		return errFakeSubscribe
	}
	return nil
}

func (this *fakePubSub) unsubscribe(ctx context.Context, channel string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	this.calls = append(this.calls, "unsub "+channel)
	if this.failUnsub {
		return errFakeUnsubscribe
	}
	return nil
}

func newTestRoomSubscriptions(fake *fakePubSub) *roomSubscriptions {
	return newRoomSubscriptions(fake.subscribe, fake.unsubscribe, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 一連串 Join / Leave 之後：呼叫了哪些 SUBSCRIBE / UNSUBSCRIBE、viewers 剩什麼
func TestRoomSubscriptionsJoinLeave(t *testing.T) {
	type step struct {
		op   string // "join" / "leave"
		room string
	}

	cases := []struct {
		name        string
		steps       []step
		wantCalls   []string
		wantViewers map[string]int
	}{
		{
			name:        "first join subscribes",
			steps:       []step{{"join", "a"}},
			wantCalls:   []string{"sub room:a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			name:        "second join does not subscribe again",
			steps:       []step{{"join", "a"}, {"join", "a"}},
			wantCalls:   []string{"sub room:a"},
			wantViewers: map[string]int{"a": 2},
		},
		{
			name:        "leave not last does not unsubscribe",
			steps:       []step{{"join", "a"}, {"join", "a"}, {"leave", "a"}},
			wantCalls:   []string{"sub room:a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			name:        "last leave unsubscribes and removes entry",
			steps:       []step{{"join", "a"}, {"leave", "a"}},
			wantCalls:   []string{"sub room:a", "unsub room:a"},
			wantViewers: map[string]int{},
		},
		{
			name:        "leave unknown room is a no-op",
			steps:       []step{{"leave", "a"}},
			wantCalls:   nil,
			wantViewers: map[string]int{},
		},
		{
			name:        "rooms are independent",
			steps:       []step{{"join", "a"}, {"join", "b"}, {"leave", "a"}},
			wantCalls:   []string{"sub room:a", "sub room:b", "unsub room:a"},
			wantViewers: map[string]int{"b": 1},
		},
		{
			name:        "rejoin after empty subscribes again",
			steps:       []step{{"join", "a"}, {"leave", "a"}, {"join", "a"}},
			wantCalls:   []string{"sub room:a", "unsub room:a", "sub room:a"},
			wantViewers: map[string]int{"a": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePubSub{}
			subs := newTestRoomSubscriptions(fake)
			ctx := context.Background()

			for _, s := range tc.steps {
				switch s.op {
				case "join":
					if err := subs.Join(ctx, s.room); err != nil {
						t.Fatalf("Join(%q) error = %v, want nil", s.room, err)
					}
				case "leave":
					subs.Leave(ctx, s.room)
				default:
					t.Fatalf("unknown op %q", s.op)
				}
			}

			if !slices.Equal(fake.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", fake.calls, tc.wantCalls)
			}
			if !maps.Equal(subs.viewers, tc.wantViewers) {
				t.Errorf("viewers = %v, want %v", subs.viewers, tc.wantViewers)
			}
		})
	}
}

// SUBSCRIBE 失敗：回 error、狀態退回（不留 entry）、盡力 UNSUBSCRIBE；之後再 Join 會重新 SUBSCRIBE
func TestRoomSubscriptionsJoinSubscribeFails(t *testing.T) {
	fake := &fakePubSub{failSub: true}
	subs := newTestRoomSubscriptions(fake)
	ctx := context.Background()

	err := subs.Join(ctx, "a")

	if !errors.Is(err, errFakeSubscribe) {
		t.Fatalf("Join error = %v, want %v", err, errFakeSubscribe)
	}
	if want := []string{"sub room:a", "unsub room:a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if len(subs.viewers) != 0 {
		t.Errorf("viewers = %v, want empty (rolled back)", subs.viewers)
	}

	// Redis 恢復後再進來：要重新 SUBSCRIBE（如果上面留了一個 viewers=0 的 entry，這裡會壞）
	fake.failSub = false
	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("second Join error = %v, want nil", err)
	}
	if want := []string{"sub room:a", "unsub room:a", "sub room:a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 最後一個人離開時 UNSUBSCRIBE 失敗：entry 一樣要刪（這台 server 上已經沒人了）
// 留著的話 viewers 卡在 1，下一個人進來只會 1→2、不會重新 SUBSCRIBE；
// 而 go-redis 的 Unsubscribe 送指令前就把 channel 從紀錄刪掉了，重連也不會訂回去 → 這間房永遠沒彈幕
func TestRoomSubscriptionsLeaveUnsubscribeFails(t *testing.T) {
	fake := &fakePubSub{}
	subs := newTestRoomSubscriptions(fake)
	ctx := context.Background()

	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("Join error = %v, want nil", err)
	}
	fake.failUnsub = true
	subs.Leave(ctx, "a")

	if len(subs.viewers) != 0 {
		t.Fatalf("viewers = %v, want empty (entry removed even though UNSUBSCRIBE failed)", subs.viewers)
	}

	// 下一個人進來：要重新 SUBSCRIBE
	fake.failUnsub = false
	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("second Join error = %v, want nil", err)
	}
	if want := []string{"sub room:a", "unsub room:a", "sub room:a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 很多連線同時進出同一個房間（用 go test -race 跑）：
// 每次 Join 都配一次 Leave，跑完之後不能留 entry，SUBSCRIBE / UNSUBSCRIBE 要成對、最後一個是 UNSUBSCRIBE
// 沒有鎖或鎖沒包住 I/O 時，這個 test 會不穩定地 fail（或被 -race 抓到）
func TestRoomSubscriptionsConcurrentJoinLeave(t *testing.T) {
	fake := &fakePubSub{}
	subs := newTestRoomSubscriptions(fake)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := subs.Join(ctx, "a"); err != nil {
				t.Errorf("Join error = %v", err)
				return
			}
			subs.Leave(ctx, "a")
		})
	}
	wg.Wait()

	if len(subs.viewers) != 0 {
		t.Errorf("viewers = %v, want empty", subs.viewers)
	}
	sub, unsub := 0, 0
	for _, c := range fake.calls {
		switch c {
		case "sub room:a":
			sub++
		case "unsub room:a":
			unsub++
		}
	}
	if sub == 0 || sub != unsub {
		t.Errorf("sub = %d, unsub = %d, want equal and > 0", sub, unsub)
	}
	// 呼叫要 sub / unsub 交替：連續兩個 sub 代表訂了兩次、連續兩個 unsub 代表重複取消
	for i, c := range fake.calls {
		want := "sub room:a"
		if i%2 == 1 {
			want = "unsub room:a"
		}
		if c != want {
			t.Fatalf("calls[%d] = %q, want %q (calls = %v)", i, c, want, fake.calls)
		}
	}
}
