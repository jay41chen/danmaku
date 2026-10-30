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
	// 「Redis 已經確認訂閱好了」的訊號：只用 close，不送值（close 一次就能叫醒所有在等的人，之後來讀的也立刻過）
	//   - 建立：addViewer 建 entry 時一起 make（mu 底下）
	//   - close：只有 confirmSubscribed（consumer 收到確認時，mu 底下）
	//   - 等：Join 在鎖外 <-ready
	ready chan struct{}
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

// 一條連線進入 roomID，回傳時 Redis 已經確認訂閱好了（join_ok 之後補撈歷史才補得到洞）
// 回傳 error = SUBSCRIBE 失敗或等確認 timeout，狀態已退回，呼叫端回 ws_join_unavailable
// 「退回」分兩種：
//   - addViewer 失敗：還沒加到人，addViewer 自己清掉 entry
//   - 等確認 timeout：人已經加了，用 Leave 退（最後一個人的話走正常的 debounce，30 秒後 expire 清掉）
func (this *roomSubscriptions) Join(ctx context.Context, roomID string) error {
	// 鎖底下：改 viewers、必要時寫出 SUBSCRIBE，拿到這間房的 ready
	ready, err := this.addViewer(ctx, roomID)
	if err != nil {
		return err
	}
	// select會等待所有列出的case，但其中一個channel case好了就可以往下走
	// 不能在鎖底下等：mu 整台 server 共用一把，等確認的這段時間所有房間的 Join / Leave 都會排隊
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		// ctx 到期（ws.go 給的 200ms）或被取消，確認還沒回來：當作沒進房，把 addViewer 加的人退掉
		// 確認之後才到也沒關係：confirmSubscribed 照樣 close ready，下一個進房的人就不用等
		this.Leave(ctx, roomID)
		return ctx.Err()
	}
}

// Join 在鎖底下的那一半（原本 Join 的內容）；回傳這間房的 ready 給 Join 在鎖外等
func (this *roomSubscriptions) addViewer(ctx context.Context, roomID string) (<-chan struct{}, error) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		// 先 subscribe、成功才放進 map 也不會漏掉確認：確認就算馬上回來，consumer 的 confirmSubscribed
		// 也要先拿 mu 才查得到 map，會等到這裡放鎖，那時 entry 已經在 map 裡
		// subscribe 失敗時 map 裡本來就沒有它，不用另外清
		newEntry := &roomSubscription{viewers: 1, ready: make(chan struct{})}
		// 找不到這個room，建立subscription
		if err := this.subscribe(ctx, roomID); err != nil {
			if uerr := this.unsubscribe(ctx, roomID); uerr != nil {
				this.logger.WarnContext(ctx, "unsubscribe after failed subscribe failed", "room", roomID, "error", uerr)
			}
			return nil, err
		}
		this.rooms[roomID] = newEntry
		// 讓最後的 return entry.ready 三個分支都能用（改 map 不會改到上面抄出來的 entry 變數）
		entry = newEntry
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
	return entry.ready, nil
}

// consumer 收到 Redis 對 SUBSCRIBE roomID 的確認時呼叫
// 同一間房可能被呼叫多次：go-redis 斷線重連後會自動重新 SUBSCRIBE 所有記住的 channel，每個都會再回一次確認
func (this *roomSubscriptions) confirmSubscribed(roomID string) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		// 確認晚到，entry 已經不在了（例如等確認 timeout → Leave → 30 秒後 expire 刪掉）：沒人在等，忽略
		return
	}
	// close 已經 close 過的 channel 會 panic，所以先非阻塞讀一次看是不是 close 過了
	// 讀完到 close 之間不會有人插進來：只有這裡會 close ready，而且握著 mu
	select {
	case <-entry.ready:
		// 讀得到 = 早就 close 過了（重連後的重複確認），不用再做事
	default:
		// 讀不到 = 還沒 close：把正在等的 Join（第一個進房的人和之後進來的人）全部叫醒
		close(entry.ready)
	}
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
