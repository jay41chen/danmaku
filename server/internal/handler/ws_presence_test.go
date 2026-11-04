package handler

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

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 這個檔案是 presenceWorker 的規格：實作要讓這些 test 全部通過

type presenceSet = map[model.PresenceEntry]struct{}

func entrySet(entries ...model.PresenceEntry) presenceSet {
	s := make(presenceSet, len(entries))
	for _, e := range entries {
		s[e] = struct{}{}
	}
	return s
}

func entry(room, conn string, user int64) model.PresenceEntry {
	return model.PresenceEntry{Room: room, ConnID: conn, UserID: user}
}

// 假 Hub：快照可以隨時換，Broadcast 記下 room + frame
type fakePresenceHub struct {
	mu         sync.Mutex
	snap       presenceSet
	broadcasts []fakeBroadcast
	broadcastC chan struct{} // 每次 Broadcast 後送一個訊號（給 run 的 test 等）
}

type fakeBroadcast struct {
	room  string
	frame []byte
}

var _ presenceHub = (*fakePresenceHub)(nil)

func newFakePresenceHub(entries ...model.PresenceEntry) *fakePresenceHub {
	return &fakePresenceHub{snap: entrySet(entries...), broadcastC: make(chan struct{}, 100)}
}

func (f *fakePresenceHub) set(entries ...model.PresenceEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = entrySet(entries...)
}

func (f *fakePresenceHub) PresenceSnapshot() map[model.PresenceEntry]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.snap)
}

func (f *fakePresenceHub) Broadcast(ctx context.Context, room string, frame []byte) {
	f.mu.Lock()
	f.broadcasts = append(f.broadcasts, fakeBroadcast{room, slices.Clone(frame)})
	f.mu.Unlock()
	f.broadcastC <- struct{}{}
}

func (f *fakePresenceHub) got() []fakeBroadcast {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.broadcasts)
}

// 假 Store：每次 SyncPresence 記 add / remove（集合），err 可以隨時換
type fakePresenceStore struct {
	mu         sync.Mutex
	syncs      []fakeSync
	syncErr    error
	counts     map[string]int
	countErr   error
	countRooms [][]string
	syncC      chan struct{} // 每次 SyncPresence 後送一個訊號
	countC     chan struct{} // 每次 CountPresence 後送一個訊號
}

type fakeSync struct {
	add, remove presenceSet
}

var _ wsPresence = (*fakePresenceStore)(nil)

func newFakePresenceStore() *fakePresenceStore {
	return &fakePresenceStore{counts: map[string]int{}, syncC: make(chan struct{}, 100), countC: make(chan struct{}, 100)}
}

func (f *fakePresenceStore) SyncPresence(ctx context.Context, add, remove []model.PresenceEntry) error {
	f.mu.Lock()
	f.syncs = append(f.syncs, fakeSync{entrySet(add...), entrySet(remove...)})
	err := f.syncErr
	f.mu.Unlock()
	f.syncC <- struct{}{}
	return err
}

func (f *fakePresenceStore) CountPresence(ctx context.Context, rooms []string) (map[string]int, error) {
	f.mu.Lock()
	f.countRooms = append(f.countRooms, slices.Clone(rooms))
	err := f.countErr
	out := maps.Clone(f.counts)
	f.mu.Unlock()
	f.countC <- struct{}{}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakePresenceStore) setSyncErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncErr = err
}

func (f *fakePresenceStore) syncCalls() []fakeSync {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

func (f *fakePresenceStore) countCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.countRooms)
}

func newTestPresenceWorker(hub *fakePresenceHub, store *fakePresenceStore) *presenceWorker {
	return newPresenceWorker(hub, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func samePresenceSet(a, b presenceSet) bool {
	return maps.Equal(a, b)
}

// 斷言第 i 次 SyncPresence 的 add / remove（集合比較，不管順序）
func expectSync(t *testing.T, store *fakePresenceStore, i int, add, remove presenceSet) {
	t.Helper()
	calls := store.syncCalls()
	if i >= len(calls) {
		t.Fatalf("SyncPresence calls = %d, want call #%d", len(calls), i)
	}
	if !samePresenceSet(calls[i].add, add) {
		t.Errorf("call #%d add = %v, want %v", i, calls[i].add, add)
	}
	if !samePresenceSet(calls[i].remove, remove) {
		t.Errorf("call #%d remove = %v, want %v", i, calls[i].remove, remove)
	}
}

func expectSyncCount(t *testing.T, store *fakePresenceStore, want int) {
	t.Helper()
	if got := len(store.syncCalls()); got != want {
		t.Fatalf("SyncPresence calls = %d, want %d", got, want)
	}
}

var (
	eA = entry("R", "c1", 0)
	eB = entry("R", "c2", 7)
	eC = entry("S", "c3", 0)
)

// 第一次 sync：快照全部寫進去，沒有東西要移除
func TestPresenceWorkerFirstSyncAddsWholeSnapshot(t *testing.T) {
	hub := newFakePresenceHub(eA, eB)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)

	w.sync(t.Context(), false)

	expectSyncCount(t, store, 1)
	expectSync(t, store, 0, entrySet(eA, eB), entrySet())
}

// 有人離開：只移除那一筆，沒變的不重寫
func TestPresenceWorkerSyncRemovesOnlyDeparted(t *testing.T) {
	hub := newFakePresenceHub(eA, eB)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(eB)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(), entrySet(eA))

	// 成功的 remove 不會重送
	w.sync(t.Context(), false)
	expectSyncCount(t, store, 2)
}

// 有人加入：只寫那一筆
func TestPresenceWorkerSyncAddsOnlyNewcomer(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(eA, eB)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(eB), entrySet())
}

// 匿名 → 登入（同一條連線身分變了）：同一次呼叫裡移除舊 member、加入新 member
func TestPresenceWorkerAnonymousToLoggedInInOneCall(t *testing.T) {
	anon := entry("R", "c1", 0)
	user := entry("R", "c1", 42)
	hub := newFakePresenceHub(anon)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(user)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(user), entrySet(anon))
}

// 換房間：舊房間的 member 要移除，新房間的要加入
func TestPresenceWorkerRoomChange(t *testing.T) {
	inR := entry("R", "c1", 5)
	inS := entry("S", "c1", 5)
	hub := newFakePresenceHub(inR)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(inS)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(inS), entrySet(inR))
}

// 續期（full=true）：沒變的也要再寫一次；remove 仍然只是 diff
func TestPresenceWorkerFullSyncRenewsEverything(t *testing.T) {
	hub := newFakePresenceHub(eA, eB, eC)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(eA, eB)
	w.sync(t.Context(), true)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(eA, eB), entrySet(eC))

	// 全量成功後 lastWritten 就是快照，接著的 diff sync 不該送任何東西
	w.sync(t.Context(), false)
	expectSyncCount(t, store, 2)
}

// 沒有任何變化時不打 Redis（add、remove 都空就不呼叫）
func TestPresenceWorkerNoChangeNoCall(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	w.sync(t.Context(), false)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 1)
}

// 快照是空的、也沒有東西要移除：不呼叫
func TestPresenceWorkerEmptySnapshotNoCall(t *testing.T) {
	store := newFakePresenceStore()
	w := newTestPresenceWorker(newFakePresenceHub(), store)

	w.sync(t.Context(), false)
	w.sync(t.Context(), true)

	expectSyncCount(t, store, 0)
}

// 失敗的呼叫裡的 remove 不能丟：下次 sync 要重送（否則 Redis 裡的幽靈 member 要等 TTL 才消失）
func TestPresenceWorkerFailedRemoveIsRetried(t *testing.T) {
	hub := newFakePresenceHub(eA, eB)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	store.setSyncErr(errors.New("redis down"))
	hub.set(eB)
	w.sync(t.Context(), false) // 這次送 remove eA，但失敗
	store.setSyncErr(nil)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 3)
	expectSync(t, store, 1, entrySet(), entrySet(eA))
	expectSync(t, store, 2, entrySet(), entrySet(eA))

	// 重送成功之後就不會再送
	w.sync(t.Context(), false)
	expectSyncCount(t, store, 3)
}

// 同一個身分離開後又回來：成功的 sync 要用快照取代 lastWritten（不是只累加），回來時才會重新 add
func TestPresenceWorkerReturningEntryIsReAdded(t *testing.T) {
	hub := newFakePresenceHub(eA, eB)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	hub.set(eB)
	w.sync(t.Context(), false)

	hub.set(eA, eB)
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 3)
	expectSync(t, store, 2, entrySet(eA), entrySet())
}

// 失敗時 lastWritten = lastWritten ∪ add、什麼都不移除：
// 失敗的 add 之後不會在 full=false 重送（等下次續期補），但 full=true 一定包含它
func TestPresenceWorkerFailedAddIsNotResentUntilFullSync(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	store.setSyncErr(errors.New("redis down"))
	hub.set(eA, eB)
	w.sync(t.Context(), false) // add eB 失敗
	store.setSyncErr(nil)
	w.sync(t.Context(), false) // 沒變化，不該重送 eB
	expectSyncCount(t, store, 2)

	w.sync(t.Context(), true)
	expectSyncCount(t, store, 3)
	expectSync(t, store, 2, entrySet(eA, eB), entrySet())
}

// full sync 失敗時一樣 lastWritten = lastWritten ∪ add，所以之後的 diff sync 不該重送已經算進去的 eB
func TestPresenceWorkerFailedFullSyncMergesAdds(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)
	w.sync(t.Context(), false)

	store.setSyncErr(errors.New("redis down"))
	hub.set(eA, eB)
	w.sync(t.Context(), true) // full sync 失敗
	store.setSyncErr(nil)
	w.sync(t.Context(), false) // 沒變化，不該有任何 store 呼叫
	expectSyncCount(t, store, 2)
}

// 失敗時要把 add 裡「每一筆」都併進 lastWritten，不是只有第一筆（曾經把 log + return 寫在 for 迴圈裡，只記到第一筆）：
// 寫入可能其實已到 Redis，漏記的人離開時不會被 ZREM，會變幽靈 member 殘留最多 30 秒
func TestPresenceWorkerFailedSyncRemembersEveryAdd(t *testing.T) {
	hub := newFakePresenceHub(eA, eB, eC)
	store := newFakePresenceStore()
	w := newTestPresenceWorker(hub, store)

	store.setSyncErr(errors.New("redis down"))
	w.sync(t.Context(), false) // 一次 add 三筆，但失敗
	store.setSyncErr(nil)
	hub.set() // 三個人都離開
	w.sync(t.Context(), false)

	expectSyncCount(t, store, 2)
	expectSync(t, store, 1, entrySet(), entrySet(eA, eB, eC))
}

// count 成功：快照裡每間房各推一個 presence frame，內容是 store 回的人數；只問快照裡出現的房間（去重）
func TestPresenceWorkerCountBroadcastsPerRoom(t *testing.T) {
	hub := newFakePresenceHub(eA, eB, eC) // R 兩條連線、S 一條
	store := newFakePresenceStore()
	store.counts = map[string]int{"R": 3, "S": 1}
	w := newTestPresenceWorker(hub, store)

	w.count(t.Context())

	calls := store.countCalls()
	if len(calls) != 1 {
		t.Fatalf("CountPresence calls = %d, want 1", len(calls))
	}
	rooms := slices.Clone(calls[0])
	slices.Sort(rooms)
	if !slices.Equal(rooms, []string{"R", "S"}) {
		t.Errorf("CountPresence rooms = %v, want [R S]", rooms)
	}
	got := map[string][]byte{}
	for _, b := range hub.got() {
		if _, dup := got[b.room]; dup {
			t.Errorf("room %q broadcast more than once", b.room)
		}
		got[b.room] = b.frame
	}
	if len(got) != 2 {
		t.Fatalf("broadcast rooms = %d, want 2", len(got))
	}
	for room, want := range map[string]int{"R": 3, "S": 1} {
		if string(got[room]) != string(encodePresence(room, want)) {
			t.Errorf("room %q frame = %s, want %s", room, got[room], encodePresence(room, want))
		}
	}
}

// count 失敗：寧可不推也不推錯的人數
func TestPresenceWorkerCountErrorBroadcastsNothing(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	store.countErr = errors.New("redis down")
	w := newTestPresenceWorker(hub, store)

	w.count(t.Context())

	if n := len(hub.got()); n != 0 {
		t.Errorf("broadcasts = %d, want 0", n)
	}
}

// presence frame 的線上格式固定：{"type":"presence","payload":{"room":..., "count":...}}
func TestEncodePresenceWireFormat(t *testing.T) {
	got := string(encodePresence("abc", 3))
	want := `{"type":"presence","payload":{"room":"abc","count":3}}`
	if got != want {
		t.Errorf("frame = %s, want %s", got, want)
	}
}

// 送進 unbuffered channel，沒人收就 fail，避免缺 select case 時卡到 go test 的 10 分鐘 timeout
func sendOrFatal[T any](t *testing.T, c chan<- T, v T, what string) {
	t.Helper()
	select {
	case c <- v:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout sending %s", what)
	}
}

func waitSignal(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// run 的接線：wake = 只寫 diff、renew = 全量續期、count = 算人數並推播、ctx 取消 = return
func TestPresenceWorkerRunDispatches(t *testing.T) {
	hub := newFakePresenceHub(eA)
	store := newFakePresenceStore()
	store.counts = map[string]int{"R": 1}
	w := newTestPresenceWorker(hub, store)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wake := make(chan struct{})
	renew := make(chan time.Time)
	count := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		w.run(ctx, wake, renew, count)
		close(done)
	}()

	// wake → diff：第一次 add eA
	sendOrFatal(t, wake, struct{}{}, "wake")
	waitSignal(t, store.syncC, "sync after wake")
	expectSync(t, store, 0, entrySet(eA), entrySet())

	// 快照多一筆，wake → 只 add 新的那筆（證明 wake 是 full=false）
	hub.set(eA, eB)
	sendOrFatal(t, wake, struct{}{}, "wake")
	waitSignal(t, store.syncC, "second sync after wake")
	expectSync(t, store, 1, entrySet(eB), entrySet())

	// renew → 全量（證明 renew 是 full=true；若當成 diff 不會有呼叫而 timeout）
	sendOrFatal(t, renew, time.Now(), "renew")
	waitSignal(t, store.syncC, "sync after renew")
	expectSync(t, store, 2, entrySet(eA, eB), entrySet())

	// 快照少一筆（eB 離開）：若 count 分支誤呼叫 sync(false)，會多出一次 remove 呼叫，下面的 expectSyncCount(3) 會抓到
	hub.set(eA)
	// count → CountPresence + Broadcast
	sendOrFatal(t, count, time.Now(), "count")
	waitSignal(t, store.countC, "count call")
	waitSignal(t, hub.broadcastC, "broadcast")
	if got := hub.got(); len(got) != 1 || got[0].room != "R" || string(got[0].frame) != string(encodePresence("R", 1)) {
		t.Errorf("broadcasts = %+v", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after ctx cancel")
	}
	// run 已 return：count tick 不該順便觸發 SyncPresence（仍是前面 3 次）
	expectSyncCount(t, store, 3)
}
