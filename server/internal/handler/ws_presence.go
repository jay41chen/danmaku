package handler

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/ws"
)

// worker 需要 Hub 的兩個能力；*ws.Hub 自動符合
type presenceHub interface {
	PresenceSnapshot() map[model.PresenceEntry]struct{}
	Broadcast(ctx context.Context, room string, frame []byte)
}

type wsPresence interface {
	SyncPresence(ctx context.Context, add, remove []model.PresenceEntry) error
	CountPresence(ctx context.Context, rooms []string) (map[string]int, error) // 去重在 redisstore 裡做
}

var _ presenceHub = (*ws.Hub)(nil)

// handler 不能 import redisstore，wsPresence 又是未匯出的，沒辦法寫編譯期檢查；接線時把 *RedisStore 傳進建構子，就是由編譯器檢查（和 wsPubSub 一樣）

type presenceWorker struct {
	hub         presenceHub
	store       wsPresence
	lastWritten map[model.PresenceEntry]struct{}
	logger      *slog.Logger
}

func newPresenceWorker(hub presenceHub, store wsPresence, logger *slog.Logger) *presenceWorker {
	return &presenceWorker{
		hub:         hub,
		store:       store,
		lastWritten: make(map[model.PresenceEntry]struct{}),
		logger:      logger,
	}
}

// 讓 Redis 裡「這台 server 寫過的成員」跟 Hub 現況一致，
//
// 概念同 Android 的 DiffUtil：
// lastWritten 是 oldList、snapshot 是 newList，算出 add / remove 後交給 SyncPresence
//
// 不能整個 key 刪掉重寫：room:R:members 是所有 server 共用的，會連別台的成員一起刪掉
// 也不去 Redis 查現況來比：查回來分不出哪些是這台寫的，所以 lastWritten 由 worker 自己記
//
//	例：lastWritten = {c1, c2}、snapshot = {c1, c3}
//	full=false（wake）   → add = {c3}、    remove = {c2}：只寫有變動的，成本 ∝ 變動量
//	full=true（每 10 秒）→ add = {c1, c3}、remove = {c2}：沒變的 c1 也要寫，score 才會更新；
//	                       不續期的話 30 秒後 c1 會被 CountPresence 當成過期刪掉
//
// 只有 run 這一個 goroutine 會呼叫，所以 lastWritten 不用鎖；
// 每次寫入前都重拿快照，不會有「舊快照晚一步寫回去」的 race（go-notes 3.27）
func (w *presenceWorker) sync(ctx context.Context, full bool) {
	snapshot := w.hub.PresenceSnapshot()

	lastWritten := w.lastWritten
	var add, remove []model.PresenceEntry

	// add：現在在、上次沒寫過
	// full 的時候不管有沒有寫過都要加入
	for s := range snapshot {
		_, ok := lastWritten[s]
		if !ok || full {
			add = append(add, s)
		}
	}
	// remove：上次寫過、現在不在了（離開、斷線）
	// 登入or換房不用另外處理
	for l := range lastWritten {
		if _, ok := snapshot[l]; !ok {
			remove = append(remove, l)
		}
	}

	// 沒有任何變動就不打 Redis（不在房間的連線登入或斷線也會 wake，但快照沒變）
	if len(add) == 0 && len(remove) == 0 {
		return
	}

	if err := w.store.SyncPresence(ctx, add, remove); err != nil {
		// 失敗（例如 timeout）時不知道 Redis 執行了沒，
		// lastWritten 的意思是「Redis 裡可能有這些、之後要負責刪」，所以只加不減
		// 等到下次tick就可以恢復正常
		for _, e := range add {
			w.lastWritten[e] = struct{}{}
		}
		w.logger.ErrorContext(ctx, "presence sync failed", "error", err)
		return
	}
	w.lastWritten = snapshot
}

// 每 5 秒算一次人數，推給這台 server 上的觀眾（別台的觀眾由別台自己推）
func (w *presenceWorker) count(ctx context.Context) {
	snapshot := w.hub.PresenceSnapshot()
	// snapshot可能會是
	//   {Room: "R", ConnID: "c1", UserID: 42}
	//   {Room: "R", ConnID: "c2", UserID: 42}
	//   {Room: "R", ConnID: "c3", UserID: 0}
	//   {Room: "S", ConnID: "c4", UserID: 7}
	// 所以要用room當成key
	roomSets := make(map[string]struct{})
	for s := range snapshot {
		roomName := s.Room
		roomSets[roomName] = struct{}{}
	}
	distinctRooms := slices.Collect(maps.Keys(roomSets))
	// 獲得每個房間名的人數
	counts, err := w.store.CountPresence(ctx, distinctRooms)
	if err != nil {
		w.logger.ErrorContext(ctx, "presence count failed", "error", err)
		return
	}
	for _, room := range distinctRooms {
		frame := encodePresence(room, counts[room])
		if frame == nil {
			continue
		}
		// 編碼成功才能送出去
		w.hub.Broadcast(ctx, room, frame)
	}
}

func (w *presenceWorker) run(ctx context.Context, wake <-chan struct{}, renew, count <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			// process 結束，直接收工
			return
		case <-wake:
			w.sync(ctx, false)
		case <-renew:
			w.sync(ctx, true)
		case <-count:
			w.count(ctx)
		}
	}
}
