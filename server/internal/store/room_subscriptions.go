package store

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// 最後一個人離開後，等多久才真的 UNSUBSCRIBE（debounce）：這段時間內有人回來就取消，不用重新 SUBSCRIBE
	// 冷門房人數在 0↔1 之間跳時，不會每次都送一對 SUBSCRIBE / UNSUBSCRIBE（見 go-notes 3.29 flapping）
	unsubscribeDelay = 30 * time.Second
	// timer 觸發時沒有 request 的 ctx 可以用，自己包一個上限，Redis 卡住時不會一直佔著鎖
	unsubscribeTimeout = 200 * time.Millisecond
)

// *time.Timer 符合這個 interface；test 換成假的，才能手動控制「時間到」
type stopper interface {
	Stop() bool
}

// 一個房間在這台 server 上的狀態。entry 存在 = 這台 server 目前有訂閱這間房
//   - viewers > 0：有人在看，timer 一定是 nil
//   - viewers == 0：沒人，正在等 unsubscribeDelay（timer 不是 nil）
type roomSubscription struct {
	viewers int
	timer   stopper
	// 每次啟動新的 timer 就 +1；timer 觸發時比對自己出生時的 gen，不一樣就代表自己已經過期
	// （跟搜尋框 debounce 比對 request id 一樣：晚到的舊 callback 直接丟掉）
	gen uint64
}

// 這台 server 上每個房間有幾條 WS 連線，決定要不要對 Redis SUBSCRIBE / UNSUBSCRIBE（lazy subscribe）
//   - viewers 0→1（沒有 entry）：SUBSCRIBE
//   - viewers 0→1（entry 還在、timer 在等）：取消 timer，不用 SUBSCRIBE
//   - viewers 1→0：啟動 timer，unsubscribeDelay 後才刪 entry + UNSUBSCRIBE
//
// 只用一把鎖，「改人數」和「送指令」包在一起（否則 X 1→0、Y 0→1 SUBSCRIBE、X 才 UNSUBSCRIBE → 有人在但沒訂）
// 不用 per-room lock：整台 server 只有一個 go-redis PubSub，它的 Subscribe / Unsubscribe
// 本來就在內部的 c.mu 排隊，per-room lock 換不到平行度（見 go-notes 3.29）
// 只算這台 server 自己身上的連線，不需要知道別台
type roomSubscriptions struct {
	mu    sync.Mutex
	rooms map[string]*roomSubscription // room public_id → 這間房的狀態；timer 觸發、確認沒人之後才刪

	// 注入：正式環境是 PubSub.Subscribe / Unsubscribe（redis.go 負責把 roomID 轉成 channel 名稱）/ time.AfterFunc，test 換成假的
	// 這裡只認 roomID，不知道 Redis channel 怎麼命名：只負責數人頭、在 0↔1 時通知外面
	subscribe   func(ctx context.Context, roomID string) error
	unsubscribe func(ctx context.Context, roomID string) error
	afterFunc   func(d time.Duration, f func()) stopper
	logger      *slog.Logger
}

func newRoomSubscriptions(
	subscribe func(ctx context.Context, roomID string) error,
	unsubscribe func(ctx context.Context, roomID string) error,
	afterFunc func(d time.Duration, f func()) stopper,
	logger *slog.Logger,
) *roomSubscriptions {
	return &roomSubscriptions{
		rooms:       make(map[string]*roomSubscription),
		subscribe:   subscribe,
		unsubscribe: unsubscribe,
		afterFunc:   afterFunc,
		logger:      logger,
	}
}

// 一條連線進入 roomID
// 回傳 error = SUBSCRIBE 失敗，狀態已退回（沒有建立 entry），呼叫端回 ws_join_unavailable
func (this *roomSubscriptions) Join(ctx context.Context, roomID string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		// 找不到這個room，建立subscription
		if err := this.subscribe(ctx, roomID); err != nil {
			if uerr := this.unsubscribe(ctx, roomID); uerr != nil {
				this.logger.WarnContext(ctx, "unsubscribe after failed subscribe failed", "room", roomID, "error", uerr)
			}
			return err
		}
		// 成功subscribe，加到map內
		this.rooms[roomID] = &roomSubscription{viewers: 1}
	} else if entry.viewers == 0 {
		// 沒有viewer => 代表有timer正在等著unsubscribe
		// Stop + 設 nil 只是省掉一次白跑（timer 觸發 → 搶鎖 → 放棄），不是正確性的來源：
		// Stop() 可能回 false（timer 已觸發、callback 在等鎖），取消不了
		// 正確性靠 expire 的 gen / viewers 檢查，不要因為這裡有 Stop 就拿掉那邊的檢查（mutation 驗過：拿掉 Stop 測試照過）
		entry.timer.Stop()
		entry.timer = nil
		entry.viewers += 1
	} else {
		// 單純加上去就好
		entry.viewers += 1
	}
	return nil
}

// 一條連線離開 roomID。不做 I/O：最後一個人離開只啟動 timer，真正的 UNSUBSCRIBE 在 expire
func (this *roomSubscriptions) Leave(ctx context.Context, roomID string) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil || entry.viewers == 0 {
		// 找不到，可能已經被其他goroutine移除了?
		this.logger.WarnContext(ctx, "leave room without join", "room", roomID)
		return
	} else if entry.viewers == 1 {
		entry.viewers = 0
		// 要記得把gen加一，以免下次關錯
		entry.gen += 1
		gen := entry.gen
		// 要開啟timer，30秒後unsubscribe
		entry.timer =
			this.afterFunc(unsubscribeDelay, func() {
				this.expire(roomID, gen)
			})
	} else {
		entry.viewers -= 1
	}
}

// gen = 這個 timer 出生時的編號
func (this *roomSubscriptions) expire(roomID string, gen uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
	defer cancel()

	// timer 觸發：在另一個 goroutine 執行（time.AfterFunc 的規則），所以一定要先拿鎖
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil || entry.gen != gen || entry.viewers > 0 {
		//	以下任一成立就 return（什麼都不做）：
		//      - 沒有 entry
		//      - entry.gen != gen（我已經過期，有更新的 timer 負責）
		//      - entry.viewers > 0（有人回來了）
		return
	}
	if err := this.unsubscribe(ctx, roomID); err != nil {
		this.logger.ErrorContext(ctx, "unsubscribe room failed", "room", roomID, "error", err)
	}
	delete(this.rooms, roomID) // 不管unsubscribe成功或失敗都要刪
}
