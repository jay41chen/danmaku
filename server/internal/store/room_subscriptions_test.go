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
	"time"

	"github.com/redis/go-redis/v9"
)

// 假的 SUBSCRIBE / UNSUBSCRIBE：依序記下收到的 roomID，例如 []string{"sub a", "unsub a"}
// failSub / failUnsub = true 時回 error（但一樣記下呼叫）
// SUBSCRIBE 成功時預設「確認馬上回來」：另開 goroutine 呼叫 confirm（= consumer 收到 *redis.Subscription）
//   - 不能直接呼叫：subscribe 是在 roomSubscriptions.mu 底下被呼叫的，confirmSubscribed 也要拿 mu，Go 的 mutex 不能重入 → 卡死
//   - manualConfirm = true：不自動確認，test 自己呼叫 subs.confirmSubscribed 決定確認什麼時候到（或永遠不到）
type fakePubSub struct {
	mu            sync.Mutex // 併發 test 用；正常情況 roomSubscriptions 已經在鎖內呼叫，多一把是保險
	calls         []string
	failSub       bool
	failUnsub     bool
	manualConfirm bool
	confirm       func(roomID string) // newTestRoomSubscriptions 接上 subs.confirmSubscribed
}

var (
	errFakeSubscribe   = errors.New("fake subscribe failed")
	errFakeUnsubscribe = errors.New("fake unsubscribe failed")
)

func (this *fakePubSub) subscribe(ctx context.Context, roomID string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	this.calls = append(this.calls, "sub "+roomID)
	if this.failSub {
		return errFakeSubscribe
	}
	if !this.manualConfirm {
		go this.confirm(roomID)
	}
	return nil
}

func (this *fakePubSub) unsubscribe(ctx context.Context, roomID string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	this.calls = append(this.calls, "unsub "+roomID)
	if this.failUnsub {
		return errFakeUnsubscribe
	}
	return nil
}

// 假的 time.AfterFunc：不會自己觸發，test 決定什麼時候「時間到」
//   - fireAll()：所有還沒停、還沒觸發的 timer 都觸發並執行 callback（= 時間過了很久）
//   - trigger(t) + run(t)：拆成兩步，模擬「timer 已經觸發（Stop 會回 false），但 callback 還在等鎖」
type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	f       func()
	stopped bool
	fired   bool
}

// 跟 *time.Timer.Stop 一樣：已經停過或已經觸發就回 false
func (this *fakeTimer) Stop() bool {
	if this.stopped || this.fired {
		return false
	}
	this.stopped = true
	return true
}

func (this *fakeClock) afterFunc(d time.Duration, f func()) stopper {
	this.mu.Lock()
	defer this.mu.Unlock()
	t := &fakeTimer{f: f}
	this.timers = append(this.timers, t)
	return t
}

// 最近一次建立的 timer（情境 test 用）
func (this *fakeClock) last() *fakeTimer {
	this.mu.Lock()
	defer this.mu.Unlock()
	return this.timers[len(this.timers)-1]
}

// 時間到了，但 callback 還沒跑（還在等鎖）：之後 Stop() 會回 false
func (this *fakeTimer) trigger() { this.fired = true }

// callback 終於拿到 CPU / 鎖
func (this *fakeTimer) run() { this.f() }

func (this *fakeClock) fireAll() {
	this.mu.Lock()
	pending := slices.Clone(this.timers)
	this.mu.Unlock()
	for _, t := range pending {
		if !t.stopped && !t.fired {
			t.trigger()
			t.run()
		}
	}
}

// 所有 Join 都要帶上限：Join 會等 SUBSCRIBE 確認，確認沒來（bug）時用 Background 會卡到 go test 的 10 分鐘 timeout，
// 帶上限的話 1 秒內就 fail 在出事的那一行
func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newTestRoomSubscriptions(fake *fakePubSub, clock *fakeClock) *roomSubscriptions {
	subs := newRoomSubscriptions(fake.subscribe, fake.unsubscribe, clock.afterFunc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fake.confirm = subs.confirmSubscribed
	return subs
}

// 每間房目前的 viewers（含 viewers = 0、還在等 timer 的 entry），方便跟 map 字面值比對
func viewerCounts(subs *roomSubscriptions) map[string]int {
	out := make(map[string]int, len(subs.rooms))
	for id, r := range subs.rooms {
		out[id] = r.viewers
	}
	return out
}

// 一連串 Join / Leave / 時間到 之後：呼叫了哪些 SUBSCRIBE / UNSUBSCRIBE、viewers 剩什麼
func TestRoomSubscriptionsJoinLeave(t *testing.T) {
	type step struct {
		op   string // "join" / "leave" / "fire"（所有等待中的 timer 時間到）
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
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			name:        "second join does not subscribe again",
			steps:       []step{{"join", "a"}, {"join", "a"}},
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 2},
		},
		{
			name:        "leave not last does not unsubscribe",
			steps:       []step{{"join", "a"}, {"join", "a"}, {"leave", "a"}},
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			name:        "last leave keeps entry until timer fires",
			steps:       []step{{"join", "a"}, {"leave", "a"}},
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 0},
		},
		{
			name:        "timer fires: unsubscribes and removes entry",
			steps:       []step{{"join", "a"}, {"leave", "a"}, {"fire", ""}},
			wantCalls:   []string{"sub a", "unsub a"},
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
			steps:       []step{{"join", "a"}, {"join", "b"}, {"leave", "a"}, {"fire", ""}},
			wantCalls:   []string{"sub a", "sub b", "unsub a"},
			wantViewers: map[string]int{"b": 1},
		},
		{
			name:        "rejoin after unsubscribed subscribes again",
			steps:       []step{{"join", "a"}, {"leave", "a"}, {"fire", ""}, {"join", "a"}},
			wantCalls:   []string{"sub a", "unsub a", "sub a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			// 離開後、時間到之前有人回來：不重新 SUBSCRIBE，之後時間到也不 UNSUBSCRIBE（timer 已取消）
			name:        "rejoin within delay cancels unsubscribe",
			steps:       []step{{"join", "a"}, {"leave", "a"}, {"join", "a"}, {"fire", ""}},
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 1},
		},
		{
			// 最後一人離開後又多一次 Leave（呼叫端 bug）：viewers 不能變成 -1，entry 和 timer 維持原狀
			name:        "leave twice without join is a no-op",
			steps:       []step{{"join", "a"}, {"leave", "a"}, {"leave", "a"}},
			wantCalls:   []string{"sub a"},
			wantViewers: map[string]int{"a": 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePubSub{}
			clock := &fakeClock{}
			subs := newTestRoomSubscriptions(fake, clock)
			ctx := testCtx(t)

			for _, s := range tc.steps {
				switch s.op {
				case "join":
					if err := subs.Join(ctx, s.room); err != nil {
						t.Fatalf("Join(%q) error = %v, want nil", s.room, err)
					}
				case "leave":
					subs.Leave(ctx, s.room)
				case "fire":
					clock.fireAll()
				default:
					t.Fatalf("unknown op %q", s.op)
				}
			}

			if !slices.Equal(fake.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", fake.calls, tc.wantCalls)
			}
			if got := viewerCounts(subs); !maps.Equal(got, tc.wantViewers) {
				t.Errorf("viewers = %v, want %v", got, tc.wantViewers)
			}
		})
	}
}

// 情境 A：timer 已經觸發、callback 在等鎖時，Y 先拿到鎖進房（Stop() 回 false，取消不了）
// → callback 拿到鎖時要看到「有人回來了」而放棄，不能在 Y 還在的時候 UNSUBSCRIBE
func TestRoomSubscriptionsTimerFiresWhileRejoining(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	mustJoin(t, subs, "a") // X
	subs.Leave(ctx, "a")   // X 離開 → timer₁
	t1 := clock.last()
	t1.trigger()           // 時間到，callback 還在等鎖
	mustJoin(t, subs, "a") // Y 先拿到鎖：Stop() 回 false
	t1.run()               // callback 終於拿到鎖

	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v (stale callback must not unsubscribe while Y is in the room)", fake.calls, want)
	}
	if got, want := viewerCounts(subs), map[string]int{"a": 1}; !maps.Equal(got, want) {
		t.Errorf("viewers = %v, want %v", got, want)
	}
}

// 情境 B：接著情境 A，Y 又離開（timer₂ 啟動），這時舊的 timer₁ 才拿到鎖
// → timer₁ 看到 viewers 0，但它已經過期（gen 不同），不能提早 UNSUBSCRIBE；要等 timer₂
func TestRoomSubscriptionsStaleTimerDoesNotUnsubscribeEarly(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	mustJoin(t, subs, "a") // X
	subs.Leave(ctx, "a")   // X 離開 → timer₁
	t1 := clock.last()     // 先存起來：Y 離開後 clock.last() 就變成 timer₂
	t1.trigger()           // 時間到，callback 還在等鎖
	mustJoin(t, subs, "a") // Y 先拿到鎖：Stop() 回 false
	subs.Leave(ctx, "a")   // Y 又離開 → timer₂
	timer2 := clock.last()
	t1.run() // 舊的 timer₁ 這時才拿到鎖：看到 viewers 0，但 gen 已經不是它的

	// timer₁ 跑完：還不能 UNSUBSCRIBE
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("after stale timer: calls = %v, want %v", fake.calls, want)
	}
	if got, want := viewerCounts(subs), map[string]int{"a": 0}; !maps.Equal(got, want) {
		t.Fatalf("after stale timer: viewers = %v, want %v", got, want)
	}

	// timer₂ 時間到：這次才 UNSUBSCRIBE
	timer2.trigger()
	timer2.run()
	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("after timer2: calls = %v, want %v", fake.calls, want)
	}
	if got := viewerCounts(subs); len(got) != 0 {
		t.Errorf("after timer2: viewers = %v, want empty", got)
	}
}

// SUBSCRIBE 失敗：回 error、狀態退回（不留 entry）、盡力 UNSUBSCRIBE；之後再 Join 會重新 SUBSCRIBE
func TestRoomSubscriptionsJoinSubscribeFails(t *testing.T) {
	fake := &fakePubSub{failSub: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	ctx := testCtx(t)

	err := subs.Join(ctx, "a")

	if !errors.Is(err, errFakeSubscribe) {
		t.Fatalf("Join error = %v, want %v", err, errFakeSubscribe)
	}
	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if len(subs.rooms) != 0 {
		t.Errorf("rooms = %v, want empty (rolled back)", viewerCounts(subs))
	}

	// Redis 恢復後再進來：要重新 SUBSCRIBE（如果上面留了一個 viewers=0 的 entry，這裡會壞）
	fake.failSub = false
	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("second Join error = %v, want nil", err)
	}
	if want := []string{"sub a", "unsub a", "sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// timer 到期時 UNSUBSCRIBE 失敗：entry 一樣要刪（這台 server 上已經沒人了）
// 留著的話下一個人進來只會走「取消 timer」那條、不會重新 SUBSCRIBE；
// 而 go-redis 的 Unsubscribe 送指令前就把 channel 從紀錄刪掉了，重連也不會訂回去 → 這間房永遠沒彈幕
func TestRoomSubscriptionsExpireUnsubscribeFails(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("Join error = %v, want nil", err)
	}
	subs.Leave(ctx, "a")
	fake.failUnsub = true
	clock.fireAll()

	if len(subs.rooms) != 0 {
		t.Fatalf("rooms = %v, want empty (entry removed even though UNSUBSCRIBE failed)", viewerCounts(subs))
	}

	// 下一個人進來：要重新 SUBSCRIBE
	fake.failUnsub = false
	if err := subs.Join(ctx, "a"); err != nil {
		t.Fatalf("second Join error = %v, want nil", err)
	}
	if want := []string{"sub a", "unsub a", "sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 很多連線同時進出同一個房間（用 go test -race 跑）：
// 每次 Join 都配一次 Leave；全部跑完、時間到之後不能留 entry，SUBSCRIBE / UNSUBSCRIBE 要成對交替
// 沒有鎖或鎖沒包住 I/O 時，這個 test 會不穩定地 fail（或被 -race 抓到）
func TestRoomSubscriptionsConcurrentJoinLeave(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

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
	clock.fireAll()

	if len(subs.rooms) != 0 {
		t.Errorf("rooms = %v, want empty", viewerCounts(subs))
	}
	sub, unsub := 0, 0
	for _, c := range fake.calls {
		switch c {
		case "sub a":
			sub++
		case "unsub a":
			unsub++
		}
	}
	if sub == 0 || sub != unsub {
		t.Errorf("sub = %d, unsub = %d, want equal and > 0", sub, unsub)
	}
	// 呼叫要 sub / unsub 交替：連續兩個 sub 代表訂了兩次、連續兩個 unsub 代表重複取消
	for i, c := range fake.calls {
		want := "sub a"
		if i%2 == 1 {
			want = "unsub a"
		}
		if c != want {
			t.Fatalf("calls[%d] = %q, want %q (calls = %v)", i, c, want, fake.calls)
		}
	}
}

// ---- 等 SUBSCRIBE 確認（ready）----
// 下面的 test 用 manualConfirm：確認什麼時候到由 test 決定

// 在背景跑 Join，回傳一個收結果的 channel（Join 會卡在等 ready，不能在 test 的 goroutine 直接呼叫）
func joinAsync(ctx context.Context, subs *roomSubscriptions, roomID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- subs.Join(ctx, roomID) }()
	return done
}

// 背景的 Join 已經在等了：viewers 達到 want 代表 addViewer 跑完、放鎖、正在 <-ready
// 用輪詢不用 sleep 固定時間：等的是「狀態到了」，不是「時間到了」
func waitViewers(t *testing.T, subs *roomSubscriptions, roomID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		subs.mu.Lock()
		entry := subs.rooms[roomID]
		got := -1
		if entry != nil {
			got = entry.viewers
		}
		subs.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewers of %q = %d, want %d", roomID, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// Join 還沒回傳（還在等確認）
func assertBlocked(t *testing.T, done <-chan error, who string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s Join returned %v before SUBSCRIBE was confirmed, want still waiting", who, err)
	case <-time.After(50 * time.Millisecond):
	}
}

// Join 回傳了，而且沒有 error
func assertJoined(t *testing.T, done <-chan error, who string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s Join error = %v, want nil", who, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s Join still waiting after confirm, want returned", who)
	}
}

// 確認回來之前 Join 不能回傳（join_ok 之後補撈歷史才補得到洞）；
// 確認還沒回來時進房的第二個人也要等同一個確認，一次 confirm 兩個都叫醒，也不會重新 SUBSCRIBE
func TestRoomSubscriptionsJoinWaitsForConfirm(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	ctx := testCtx(t)

	a := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	assertBlocked(t, a, "A")

	b := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 2)
	assertBlocked(t, b, "B")

	subs.confirmSubscribed("a")

	assertJoined(t, a, "A")
	assertJoined(t, b, "B")
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 等確認 timeout：回 ctx 的 error，剛加的人要退掉（走 Leave → viewers 0、啟動 timer，entry 留著）
// 確認之後才到：不 panic，close ready；下一個人進房不用再等、也不重新 SUBSCRIBE
func TestRoomSubscriptionsJoinConfirmTimeout(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := subs.Join(ctx, "a")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Join error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got, want := viewerCounts(subs), map[string]int{"a": 0}; !maps.Equal(got, want) {
		t.Fatalf("viewers = %v, want %v (timed-out viewer rolled back via Leave)", got, want)
	}
	if len(clock.timers) != 1 {
		t.Fatalf("timers = %d, want 1 (Leave of the last viewer starts the unsubscribe delay)", len(clock.timers))
	}

	// 確認晚到
	subs.confirmSubscribed("a")

	// 下一個人：ready 已經 close，不用等；entry 還在，不重新 SUBSCRIBE
	next := joinAsync(testCtx(t), subs, "a")
	assertJoined(t, next, "next")
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 確認一直沒來，timeout 的人退掉後 30 秒到期：照樣 UNSUBSCRIBE、刪 entry（不會因為沒確認就漏掉清理）
// 之後確認才到（entry 已經不在）：忽略，不 panic、不建 entry
func TestRoomSubscriptionsConfirmAfterExpireIsIgnored(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := subs.Join(ctx, "a"); err == nil {
		t.Fatal("Join error = nil, want timeout")
	}
	clock.fireAll()

	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}

	subs.confirmSubscribed("a")

	if len(subs.rooms) != 0 {
		t.Errorf("rooms = %v, want empty", viewerCounts(subs))
	}
}

// 同一間房收到多次確認（go-redis 斷線重連後會自動重新 SUBSCRIBE）：不能 close 兩次（會 panic）
// 不認識的房間收到確認：忽略
func TestRoomSubscriptionsConfirmTwiceDoesNotPanic(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})

	a := joinAsync(testCtx(t), subs, "a")
	waitViewers(t, subs, "a", 1)
	subs.confirmSubscribed("a")
	assertJoined(t, a, "A")

	subs.confirmSubscribed("a") // 重連後的重複確認
	subs.confirmSubscribed("x") // 沒有 entry 的房間

	if got, want := viewerCounts(subs), map[string]int{"a": 1}; !maps.Equal(got, want) {
		t.Errorf("viewers = %v, want %v", got, want)
	}
}

// debounce 期間有人回來：entry（和已經 close 的 ready）還在，Join 立刻回傳，不用再等確認
func TestRoomSubscriptionsRejoinWithinDelayDoesNotWait(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	ctx := testCtx(t)

	a := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	subs.confirmSubscribed("a")
	assertJoined(t, a, "A")
	subs.Leave(ctx, "a")

	// 不再 confirm：如果 Join 要等新的確認，這裡會卡住
	again := joinAsync(ctx, subs, "a")
	assertJoined(t, again, "rejoin")
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// consumer 收到 unsubscribe 的確認：不能拿去叫醒等 SUBSCRIBE 確認的人
// 情境：expire 送出 UNSUBSCRIBE、刪掉 entry 之後，X 馬上進房建了新 entry；舊的 UNSUBSCRIBE 確認這時才到（時間軸見 handleSubscription）
func TestHandleSubscriptionIgnoresUnsubscribe(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	store := &RedisStore{rooms: subs, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := testCtx(t)

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)

	store.handleSubscription(ctx, &redis.Subscription{Kind: "unsubscribe", Channel: roomChannel("a")})
	assertBlocked(t, x, "X")

	store.handleSubscription(ctx, &redis.Subscription{Kind: "subscribe", Channel: roomChannel("a")})
	assertJoined(t, x, "X")
}

func mustJoin(t *testing.T, subs *roomSubscriptions, roomID string) {
	t.Helper()
	if err := subs.Join(testCtx(t), roomID); err != nil {
		t.Fatalf("Join(%q) error = %v, want nil", roomID, err)
	}
}
