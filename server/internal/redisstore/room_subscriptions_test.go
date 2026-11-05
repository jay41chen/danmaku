package redisstore

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
// SUBSCRIBE 成功時預設「確認馬上回來」：送完直接呼叫 confirm（= consumer 收到 *redis.Subscription）
//   - 可以直接呼叫：worker 送指令時沒拿著 roomSubscriptions.mu（以前在鎖底下送，要另開 goroutine 才不會卡死）
//   - manualConfirm = true：不自動確認，test 自己呼叫 subs.confirmSubscribed 決定確認什麼時候到（或永遠不到）
//   - beforeSub / beforeUnsub：送指令「進行中」的 hook，test 用來讓 I/O 卡住、在 I/O 期間插入別的操作
type fakePubSub struct {
	mu            sync.Mutex // 併發 test 用：背景 worker 寫 calls，test 讀
	calls         []string
	failSub       bool
	failUnsub     bool
	manualConfirm bool
	confirm       func(roomID string) // newTestRoomSubscriptions 接上 subs.confirmSubscribed
	beforeSub     func(roomID string)
	beforeUnsub   func(roomID string)
}

var (
	errFakeSubscribe   = errors.New("fake subscribe failed")
	errFakeUnsubscribe = errors.New("fake unsubscribe failed")
)

func (this *fakePubSub) subscribe(ctx context.Context, roomID string) error {
	if this.beforeSub != nil {
		this.beforeSub(roomID)
	}
	this.mu.Lock()
	this.calls = append(this.calls, "sub "+roomID)
	fail, manual := this.failSub, this.manualConfirm
	this.mu.Unlock()
	if fail {
		return errFakeSubscribe
	}
	if !manual {
		// 放掉 fake.mu 再確認：confirmSubscribed 要拿 subs.mu，不要兩把鎖疊在一起
		this.confirm(roomID)
	}
	return nil
}

func (this *fakePubSub) unsubscribe(ctx context.Context, roomID string) error {
	if this.beforeUnsub != nil {
		this.beforeUnsub(roomID)
	}
	this.mu.Lock()
	defer this.mu.Unlock()
	this.calls = append(this.calls, "unsub "+roomID)
	if this.failUnsub {
		return errFakeUnsubscribe
	}
	return nil
}

// 背景 worker 還在跑時也能安全讀
func (this *fakePubSub) snapshot() []string {
	this.mu.Lock()
	defer this.mu.Unlock()
	return slices.Clone(this.calls)
}

func (this *fakePubSub) setFailSub(fail bool) {
	this.mu.Lock()
	defer this.mu.Unlock()
	this.failSub = fail
}

// 假的 time.AfterFunc：不會自己觸發，test 決定什麼時候「時間到」
//   - fireAll()：所有還沒停、還沒觸發的 timer 都觸發並執行 callback（= 時間過了很久）
//   - fire(d)：只觸發等待時間是 d 的（分辨 debounce / 確認 / backoff 三種 timer）
//   - trigger(t) + run(t)：拆成兩步，模擬「timer 已經觸發（Stop 會回 false），但 callback 還在等鎖」
type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
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
	t := &fakeTimer{d: d, f: f}
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

// 還沒停、還沒觸發的 timer；d = 0 表示不分種類
func (this *fakeClock) pending(d time.Duration) []*fakeTimer {
	this.mu.Lock()
	defer this.mu.Unlock()
	var out []*fakeTimer
	for _, t := range this.timers {
		if !t.stopped && !t.fired && (d == 0 || t.d == d) {
			out = append(out, t)
		}
	}
	return out
}

func (this *fakeClock) fireAll() { this.fire(0) }

func (this *fakeClock) fire(d time.Duration) {
	for _, t := range this.pending(d) {
		t.trigger()
		t.run()
	}
}

// 所有 Join 都要帶上限：Join 會等 SUBSCRIBE 確認，確認沒來（bug）時用 Background 會卡到 go test 的 10 分鐘 timeout，
// 帶上限的話 1 秒內就 fail 在出事的那一行
func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

// 不啟動 worker：大部分 test 用 drain 在 test 的 goroutine 裡同步跑 worker，結果才是確定的
// 要測真正的背景 worker（run）時用 startWorker；同一個 subs 不能兩個都用（會變成兩個 worker）
func newTestRoomSubscriptions(fake *fakePubSub, clock *fakeClock) *roomSubscriptions {
	subs := newRoomSubscriptions(fake.subscribe, fake.unsubscribe, clock.afterFunc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fake.confirm = func(roomID string) { subs.confirmSubscribed(roomID) }
	return subs
}

// 同步跑 worker 直到沒有 dirty 的房間（跟 run 的迴圈本體一樣，只是不等 wake）
// reconcile 最後會把房間再標一次，所以要跑到 dirty 空了才停；超過上限代表 worker 一直自己叫醒自己（bug）
// 用 t.Errorf 不用 t.Fatalf：有的 test 在別的 goroutine 呼叫 drain
func drain(t *testing.T, subs *roomSubscriptions) {
	t.Helper()
	for range 100 {
		subs.mu.Lock()
		dirties := subs.dirty
		subs.dirty = make(map[string]struct{})
		subs.mu.Unlock()
		if len(dirties) == 0 {
			return
		}
		for roomID := range dirties {
			subs.reconcile(t.Context(), roomID)
		}
	}
	t.Errorf("drain: still dirty after 100 rounds (worker keeps waking itself up)")
}

// 背景跑真正的 worker；test 結束時取消 ctx，並確認 run 有 return
func startWorker(t *testing.T, subs *roomSubscriptions) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		subs.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("run did not return after ctx was canceled")
		}
	})
}

// 每間房目前的 viewers（含 viewers = 0、還在等 timer 的 entry），方便跟 map 字面值比對
func viewerCounts(subs *roomSubscriptions) map[string]int {
	subs.mu.Lock()
	defer subs.mu.Unlock()
	out := make(map[string]int, len(subs.rooms))
	for id, r := range subs.rooms {
		out[id] = r.viewers
	}
	return out
}

func stateOf(subs *roomSubscriptions, roomID string) (subState, bool) {
	subs.mu.Lock()
	defer subs.mu.Unlock()
	entry := subs.rooms[roomID]
	if entry == nil {
		return 0, false
	}
	return entry.state, true
}

// 一連串 Join / Leave / 時間到 之後：呼叫了哪些 SUBSCRIBE / UNSUBSCRIBE、viewers 剩什麼
// 每一步之後都 drain：讓 worker 把該送的指令送完再做下一步
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
					mustJoin(t, subs, s.room)
				case "leave":
					subs.Leave(ctx, s.room)
				case "fire":
					clock.fireAll()
				default:
					t.Fatalf("unknown op %q", s.op)
				}
				drain(t, subs)
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
	drain(t, subs)

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
	drain(t, subs)

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
	drain(t, subs)
	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("after timer2: calls = %v, want %v", fake.calls, want)
	}
	if got := viewerCounts(subs); len(got) != 0 {
		t.Errorf("after timer2: viewers = %v, want empty", got)
	}
}

// SUBSCRIBE 失敗：Join 繼續等（不是馬上回 error），worker 在背景 backoff 重試：100ms → 200ms → …
// Redis 恢復後重試成功 → 確認 → 等著的 Join 回傳；失敗次數歸零
func TestRoomSubscriptionsSubscribeFailsRetriesWithBackoff(t *testing.T) {
	fake := &fakePubSub{failSub: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	x := joinAsync(testCtx(t), subs, "a")
	waitViewers(t, subs, "a", 1)

	// 第 1 次失敗：排 100ms 後重試，不會立刻重試
	drain(t, subs)
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("after 1st failure: calls = %v, want %v (must not retry immediately)", fake.calls, want)
	}
	if n := len(clock.pending(retryBaseDelay)); n != 1 {
		t.Fatalf("after 1st failure: pending %v timers = %d, want 1", retryBaseDelay, n)
	}
	assertBlocked(t, x, "X")

	// 第 2 次失敗：等待時間翻倍
	clock.fire(retryBaseDelay)
	drain(t, subs)
	if n := len(clock.pending(2 * retryBaseDelay)); n != 1 {
		t.Fatalf("after 2nd failure: pending %v timers = %d, want 1", 2*retryBaseDelay, n)
	}

	// Redis 恢復
	fake.setFailSub(false)
	clock.fire(2 * retryBaseDelay)
	drain(t, subs)
	assertJoined(t, x, "X")
	if want := []string{"sub a", "sub a", "sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	subs.mu.Lock()
	failures := subs.rooms["a"].failures
	subs.mu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d, want 0 (reset after a successful SUBSCRIBE)", failures)
	}
}

func TestRetryBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{6, 3200 * time.Millisecond},
		{7, retryMaxDelay},    // 6400ms 超過上限
		{1000, retryMaxDelay}, // 用位移算的話會溢位
	}
	for _, tc := range cases {
		if got := retryBackoff(tc.failures); got != tc.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// SUBSCRIBE 之後一直沒有離開的人、也沒確認：Join timeout 退掉，30 秒後要 UNSUBSCRIBE
// SUB 失敗也一樣：go-redis 的 Subscribe 寫入失敗也會把 channel 記下來（pubsub.go Subscribe），重連時會自己訂回去
// → 不送 UNSUBSCRIBE 就刪 entry 的話，這台 server 會一直收這間房的訊息
func TestRoomSubscriptionsLeaveAfterFailedSubscribeUnsubscribes(t *testing.T) {
	fake := &fakePubSub{failSub: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs) // SUB 失敗，排 backoff
	if err := <-x; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Join error = %v, want %v", err, context.DeadlineExceeded)
	}

	clock.fire(unsubscribeDelay) // debounce 到期
	drain(t, subs)

	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v (go-redis remembers the channel even when SUBSCRIBE failed)", fake.calls, want)
	}
	if got := viewerCounts(subs); len(got) != 0 {
		t.Errorf("viewers = %v, want empty", got)
	}
}

// timer 到期時 UNSUBSCRIBE 失敗：entry 一樣要刪（這台 server 上已經沒人了），不重試
// go-redis 的 Unsubscribe 送指令前就把 channel 從紀錄刪掉了，重連也不會訂回去 → 視為已取消
// 留著的話下一個人進來不會重新 SUBSCRIBE
func TestRoomSubscriptionsExpireUnsubscribeFails(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	mustJoin(t, subs, "a")
	subs.Leave(ctx, "a")
	fake.failUnsub = true
	clock.fireAll()
	drain(t, subs)

	if got := viewerCounts(subs); len(got) != 0 {
		t.Fatalf("rooms = %v, want empty (entry removed even though UNSUBSCRIBE failed)", got)
	}

	// 下一個人進來：要重新 SUBSCRIBE
	fake.failUnsub = false
	mustJoin(t, subs, "a")
	if want := []string{"sub a", "unsub a", "sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 很多連線同時進出同一個房間（用 go test -race 跑），背景跑真正的 worker（run）：
// 每次 Join 都配一次 Leave；期間 debounce 沒到期，所以只會 SUBSCRIBE 一次；
// 全部跑完、時間到之後 UNSUBSCRIBE 一次、entry 刪掉
func TestRoomSubscriptionsConcurrentJoinLeave(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	startWorker(t, subs)
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

	waitFor(t, "entry removed", func() bool { return len(viewerCounts(subs)) == 0 })
	if got, want := fake.snapshot(), []string{"sub a", "unsub a"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// B1：worker 送指令卡住時（Redis 變黑洞），Join / Leave 不能跟著卡
// 卡住的只有 worker：已經訂好的房間照常進出；新房間要等 worker 空下來才會送 SUB（已知限制：單一 worker）
func TestRoomSubscriptionsStuckSubscribeDoesNotBlockJoinLeave(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	startWorker(t, subs)
	ctx := testCtx(t)

	mustJoinBackground(t, subs, "a") // 先訂好 a

	entered := make(chan struct{})
	release := make(chan struct{})
	fake.beforeSub = func(roomID string) {
		if roomID == "slow" {
			close(entered)
			<-release
		}
	}
	slow := joinAsync(ctx, subs, "slow")
	<-entered // worker 正卡在送 SUBSCRIBE slow

	// 卡住期間：a 的 Join / Leave 要馬上完成（以前拿著 mu 送指令，這裡會一起卡住）
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := subs.Join(ctx, "a"); err != nil {
			t.Errorf("Join(a) error = %v", err)
		}
		subs.Leave(ctx, "a")
		subs.Leave(ctx, "a")
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Join / Leave of another room blocked while the worker was stuck")
	}

	close(release)
	assertJoined(t, slow, "slow")
}

// ---- 等 SUBSCRIBE 確認（ready）----

// 在背景跑 Join，回傳一個收結果的 channel（Join 會卡在等 ready，不能在 test 的 goroutine 直接呼叫）
func joinAsync(ctx context.Context, subs *roomSubscriptions, roomID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- subs.Join(ctx, roomID) }()
	return done
}

// 輪詢到 cond 成立（等的是「狀態到了」，不是「時間到了」）
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// 背景的 Join 已經在等了：viewers 達到 want 代表 addViewer 跑完、放鎖、正在 <-ready
func waitViewers(t *testing.T, subs *roomSubscriptions, roomID string, want int) {
	t.Helper()
	waitFor(t, "viewers of "+roomID, func() bool {
		got, ok := viewerCounts(subs)[roomID]
		return ok && got == want
	})
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
	drain(t, subs)
	assertBlocked(t, a, "A")

	b := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 2)
	drain(t, subs)
	assertBlocked(t, b, "B")

	subs.confirmSubscribed("a")

	assertJoined(t, a, "A")
	assertJoined(t, b, "B")
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 等確認 timeout：回 ctx 的 error，剛加的人要退掉（走 Leave → viewers 0、啟動 debounce timer，entry 留著）
// 確認之後才到：close ready；下一個人進房不用再等、也不重新 SUBSCRIBE
func TestRoomSubscriptionsJoinConfirmTimeout(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)

	if err := <-x; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Join error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got, want := viewerCounts(subs), map[string]int{"a": 0}; !maps.Equal(got, want) {
		t.Fatalf("viewers = %v, want %v (timed-out viewer rolled back via Leave)", got, want)
	}
	if n := len(clock.pending(unsubscribeDelay)); n != 1 {
		t.Fatalf("debounce timers = %d, want 1 (Leave of the last viewer starts the unsubscribe delay)", n)
	}

	// 確認晚到
	subs.confirmSubscribed("a")

	// 下一個人：ready 已經 close，不用等；entry 還在，不重新 SUBSCRIBE
	next := joinAsync(testCtx(t), subs, "a")
	assertJoined(t, next, "next")
	drain(t, subs)
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// 送出 SUB 之後確認一直沒來（被延遲或丟掉）：confirmResendDelay 後重送 SUB，新的確認到了 Join 才回傳
func TestRoomSubscriptionsConfirmTimeoutResends(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	x := joinAsync(testCtx(t), subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)

	clock.fire(confirmResendDelay)
	drain(t, subs)
	if want := []string{"sub a", "sub a"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %v, want %v (resend after confirm timeout)", fake.calls, want)
	}
	assertBlocked(t, x, "X")

	subs.confirmSubscribed("a")
	assertJoined(t, x, "X")
}

// 確認已經到了，確認 timer 才觸發：不重送
func TestRoomSubscriptionsConfirmTimerAfterConfirmDoesNothing(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)

	mustJoin(t, subs, "a")
	clock.fire(confirmResendDelay)
	drain(t, subs)

	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if state, _ := stateOf(subs, "a"); state != subConfirmed {
		t.Errorf("state = %v, want subConfirmed", state)
	}
}

// worker 正在送 UNSUBSCRIBE 時 X 進房：X 不能拿到舊的（早就 close 的）ready 立刻 join_ok，
// 要等 worker 重新 SUBSCRIBE、確認回來才回傳
func TestRoomSubscriptionsJoinDuringUnsubscribeWaitsForNewConfirm(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	mustJoin(t, subs, "a")
	subs.Leave(ctx, "a")
	clock.fire(unsubscribeDelay) // expire：標 dirty

	entered := make(chan struct{})
	release := make(chan struct{})
	fake.beforeUnsub = func(string) {
		close(entered)
		<-release
	}
	drained := make(chan struct{})
	go func() {
		drain(t, subs)
		close(drained)
	}()
	<-entered // worker 正卡在送 UNSUBSCRIBE

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	assertBlocked(t, x, "X")

	close(release)
	<-drained
	assertJoined(t, x, "X")
	if want := []string{"sub a", "unsub a", "sub a"}; !slices.Equal(fake.calls, want) {
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
	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)
	if err := <-x; err == nil {
		t.Fatal("Join error = nil, want timeout")
	}
	clock.fireAll() // 確認 timer + debounce timer 都到期
	drain(t, subs)

	if want := []string{"sub a", "unsub a"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}

	subs.confirmSubscribed("a")

	if got := viewerCounts(subs); len(got) != 0 {
		t.Errorf("rooms = %v, want empty", got)
	}
}

// 同一間房收到多次確認（go-redis 斷線重連後會自動重新 SUBSCRIBE）：不能 close 兩次（會 panic）
// 不認識的房間收到確認：忽略
func TestRoomSubscriptionsConfirmTwiceDoesNotPanic(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})

	a := joinAsync(testCtx(t), subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)
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
	drain(t, subs)
	subs.confirmSubscribed("a")
	assertJoined(t, a, "A")
	subs.Leave(ctx, "a")

	// 不再 confirm、也不 drain：如果 Join 要等新的確認，這裡會卡住
	again := joinAsync(ctx, subs, "a")
	assertJoined(t, again, "rejoin")
	drain(t, subs)
	if want := []string{"sub a"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// consumer 收到 unsubscribe 的確認：不能拿去叫醒等 SUBSCRIBE 確認的人（時間軸見 handleSubscription）
func TestHandleSubscriptionIgnoresUnsubscribe(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	store := &RedisStore{rooms: subs, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := testCtx(t)

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs) // SUB 送出，state = subSent

	store.handleSubscription(ctx, &redis.Subscription{Kind: "unsubscribe", Channel: roomChannel("a")}, func(string) {})
	assertBlocked(t, x, "X")

	store.handleSubscription(ctx, &redis.Subscription{Kind: "subscribe", Channel: roomChannel("a")}, func(string) {})
	assertJoined(t, x, "X")
}

// Join 並等到回傳；沒有背景 worker，所以等的同時自己 drain（Join 要等 worker 送 SUB、確認回來）
func mustJoin(t *testing.T, subs *roomSubscriptions, roomID string) {
	t.Helper()
	done := joinAsync(testCtx(t), subs, roomID)
	for {
		drain(t, subs)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Join(%q) error = %v, want nil", roomID, err)
			}
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// 有背景 worker（startWorker）時用：直接等 Join 回傳
func mustJoinBackground(t *testing.T, subs *roomSubscriptions, roomID string) {
	t.Helper()
	if err := subs.Join(testCtx(t), roomID); err != nil {
		t.Fatalf("Join(%q) error = %v, want nil", roomID, err)
	}
}

// PubSub 換新之後，已確認的房間不能再算「已確認」：重建後進房的人要等新連線的確認，
// 不然會拿到舊的、早就 close 的 ready → 立刻 join_ok，但新連線根本還沒訂 → 漏訊息
func TestResetAfterRebuildNewJoinWaitsForReconfirm(t *testing.T) {
	fake := &fakePubSub{}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	ctx := testCtx(t)
	mustJoin(t, subs, "a")

	subs.resetAfterRebuild(time.Now())
	fake.mu.Lock()
	fake.manualConfirm = true
	fake.mu.Unlock()
	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 2)
	drain(t, subs)
	assertBlocked(t, x, "X")

	subs.confirmSubscribed("a")
	assertJoined(t, x, "X")
}

// 重設之後 worker 對還有人在看的房間重送 SUBSCRIBE（新連線什麼都沒訂）
func TestResetAfterRebuildWorkerResendsSubscribe(t *testing.T) {
	fake := &fakePubSub{}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	mustJoin(t, subs, "a")

	subs.resetAfterRebuild(time.Now())
	drain(t, subs)

	if want := []string{"sub a", "sub a"}; !slices.Equal(fake.snapshot(), want) {
		t.Errorf("calls = %v, want %v", fake.snapshot(), want)
	}
}

// resync 每房只回報一次（since = 重設的時間）；沒有 reset 過的正常確認不回報
// 「只報一次」要靠 confirmSubscribed 清掉 resyncSince 才成立：第二次確認前要先讓 state 回到 subSent
// （同一個 entry：Leave、expire、worker 送 UNSUB 的當下有人重新 Join，worker 再送 SUB），
// 否則第二次確認因為 state 已是 subConfirmed 提早回 false，測不到有沒有清
func TestConfirmSubscribedReportsResyncOnlyOncePerReset(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)
	if resync, _ := subs.confirmSubscribed("a"); resync {
		t.Error("normal confirm reported resync, want false")
	}
	assertJoined(t, x, "X")

	resetAt := time.Now()
	subs.resetAfterRebuild(resetAt)
	drain(t, subs) // 重送 SUB，state = subSent
	resync, since := subs.confirmSubscribed("a")
	if !resync || !since.Equal(resetAt) {
		t.Errorf("confirm after reset = (%v, %v), want (true, %v)", resync, since, resetAt)
	}
	if resync, _ := subs.confirmSubscribed("a"); resync {
		t.Error("second confirm reported resync again, want once per reset")
	}

	// 走一條回到 subSent 的路徑：X 離開、expire，worker 送 UNSUB 的當下 Y 進房，worker 再送 SUB
	subs.Leave(ctx, "a")
	clock.fire(unsubscribeDelay)
	var y <-chan error
	fake.beforeUnsub = func(string) {
		y = joinAsync(ctx, subs, "a")
		waitViewers(t, subs, "a", 1)
	}
	drain(t, subs)
	if st, ok := stateOf(subs, "a"); !ok || st != subSent {
		t.Fatalf("state = (%v, %v), want subSent on the same entry", st, ok)
	}
	if want := []string{"sub a", "sub a", "unsub a", "sub a"}; !slices.Equal(fake.snapshot(), want) {
		t.Fatalf("calls = %v, want %v", fake.snapshot(), want)
	}
	if resync, _ := subs.confirmSubscribed("a"); resync {
		t.Error("confirm after rejoin reported resync again, resyncSince was not cleared")
	}
	assertJoined(t, y, "Y")

	// 再重建一次：又可以回報一次
	subs.resetAfterRebuild(time.Now())
	drain(t, subs)
	if resync, _ := subs.confirmSubscribed("a"); !resync {
		t.Error("confirm after second reset did not report resync")
	}
}

// debounce 中（viewers = 0、timer 還在）的房間：reset 之後 worker 仍要重送 SUB（desired 還是要訂），
// 但確認時不回報 resync：房裡沒有人可通知，之後回來的人要等 ready，join_ok 後本來就會 GET 歷史
func TestResetAfterRebuildDebouncingRoomResubscribesWithoutResync(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	ctx := testCtx(t)

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs)
	subs.confirmSubscribed("a")
	assertJoined(t, x, "X")
	subs.Leave(ctx, "a") // viewers 0、timer 還在

	subs.resetAfterRebuild(time.Now())
	drain(t, subs)

	if want := []string{"sub a", "sub a"}; !slices.Equal(fake.snapshot(), want) {
		t.Errorf("calls = %v, want %v", fake.snapshot(), want)
	}
	if resync, _ := subs.confirmSubscribed("a"); resync {
		t.Error("confirm of a room with no viewers reported resync, want false")
	}
}

// 沒人要的房間（viewers = 0、timer = nil，等 worker 刪）：reset 不刪 entry、不重送 SUB，由 worker 刪掉
// 也不送 UNSUB：新連線本來就沒訂這間房
func TestResetAfterRebuildUnwantedRoomIsDeletedWithoutSubscribe(t *testing.T) {
	fake := &fakePubSub{}
	clock := &fakeClock{}
	subs := newTestRoomSubscriptions(fake, clock)
	mustJoin(t, subs, "a")
	subs.Leave(testCtx(t), "a")
	clock.fire(unsubscribeDelay) // expire：viewers 0、timer nil，還沒 drain

	subs.resetAfterRebuild(time.Now())
	if _, ok := stateOf(subs, "a"); !ok {
		t.Fatal("reset deleted the entry, want only the worker to delete")
	}
	drain(t, subs)

	if _, ok := stateOf(subs, "a"); ok {
		t.Error("entry still exists after worker ran, want deleted")
	}
	if want := []string{"sub a"}; !slices.Equal(fake.snapshot(), want) {
		t.Errorf("calls = %v, want %v", fake.snapshot(), want)
	}
}

// 還在等確認的 Join（subSent，ready 還沒 close）：reset 不能換掉它的 ready，
// 否則這個人永遠等不到；重送 SUB 的確認到達時要能叫醒它，並回報 resync
func TestResetAfterRebuildKeepsPendingReadyAndWakesIt(t *testing.T) {
	fake := &fakePubSub{manualConfirm: true}
	subs := newTestRoomSubscriptions(fake, &fakeClock{})
	ctx := testCtx(t)

	x := joinAsync(ctx, subs, "a")
	waitViewers(t, subs, "a", 1)
	drain(t, subs) // subSent
	assertBlocked(t, x, "X")

	subs.resetAfterRebuild(time.Now())
	drain(t, subs) // 重送 SUB
	assertBlocked(t, x, "X")

	if resync, _ := subs.confirmSubscribed("a"); !resync {
		t.Error("confirm after reset did not report resync")
	}
	assertJoined(t, x, "X")
	if want := []string{"sub a", "sub a"}; !slices.Equal(fake.snapshot(), want) {
		t.Errorf("calls = %v, want %v", fake.snapshot(), want)
	}
}
