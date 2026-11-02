package store

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// roomSubscriptions：這台 server 上每個房間有幾條 WS 連線，決定要不要對 Redis SUBSCRIBE / UNSUBSCRIBE（lazy subscribe）
//
// 兩個角色（分兩個檔案）：
//   - 事件端（這個檔案）：Join / Leave / expire / confirmSubscribed，很多 goroutine 呼叫，
//     在 mu 底下只改記憶體（viewers、timer、state），需要的話把房間標成 dirty，不做 I/O
//   - worker（room_subscriptions_worker.go）：整台 server 一個 goroutine，把 dirty 的房間拿出來，
//     比對 desired / actual，不一樣就在鎖外送 SUB / UNSUB（reconciliation loop，同 Kubernetes controller）
//
// 為什麼不在鎖底下送指令（原本的做法）：Redis 變成黑洞時 go-redis 拿著自己的鎖重連好幾秒，
// 拿著 mu 的人跟著等 → 整台 server 的 Join / Leave（純記憶體的也是）全部卡住（實測 5–15 秒）；
// 而且 sync.Mutex 不吃 ctx，200ms timeout 管不到等鎖的時間
//
// 順序怎麼保證（原本靠「改人數和送指令包在同一把鎖」）：只有一個 worker 送指令，
// 而且它每次都以「當下的 desired」為準，不照事件重播 → 不會出現 UNSUB / SUB 順序顛倒；
// 中間來回的變化（SUB、UNSUB、SUB…）自然被合併成一個指令
//
// 不用 per-room lock：要的是隔離不是平行度（go-redis 一次只處理一個 SUB / UNSUB），
// 而且兩層鎖會遇到「放掉全域鎖、拿到房間鎖之前 entry 被刪」的 TOCTOU
//
// 只算這台 server 自己身上的連線，不需要知道別台

// 最後一個人離開後，等多久才讓 worker 送 UNSUBSCRIBE（debounce）：這段時間內有人回來就取消，不用重新 SUBSCRIBE
// 冷門房人數在 0↔1 之間跳時，不會每次都送一對 SUBSCRIBE / UNSUBSCRIBE（見 go-notes 3.29 flapping）
const unsubscribeDelay = 30 * time.Second

// *time.Timer 符合這個 interface；test 換成假的，才能手動控制「時間到」
type stopper interface {
	Stop() bool
}

// actual：Redis 那邊（我們認為）現在的狀態。desired 不用存，從 viewers / timer 推出來
type subState int

const (
	subIdle      subState = iota // 沒訂，或當作沒訂（已決定 UNSUB）；零值
	subSent                      // SUB 寫出去了，等 Redis 確認
	subConfirmed                 // Redis 確認了（ready 已 close）
	subRetry                     // SUB 送過但沒收到確認（寫入失敗、確認逾時）→ Redis / go-redis「可能」已經訂了
)

// 一個房間在這台 server 上的狀態。entry 存在 = 這間房 desired 或 actual 至少一個是「有訂」
//   - desired（應該訂嗎）= viewers > 0 || timer != nil：有人在看，或還在 unsubscribeDelay 的 debounce 裡
//   - actual（Redis 那邊）= state
//   - 只有 worker 會刪 entry，而且只在 desired、actual 都是「沒訂」的時候
//     （expire 不能刪：刪掉的話「Redis 那邊還訂著」這件事就沒地方記了）
type roomSubscription struct {
	viewers int
	timer   stopper // debounce timer：viewers > 0 時一定是 nil。只放 debounce timer，確認 / backoff timer 不能放這裡（會被算進 desired）
	// 每次啟動新的 timer 就 +1；timer 觸發時比對自己出生時的 gen，不一樣就代表自己已經過期
	// （跟搜尋框 debounce 比對 request id 一樣：晚到的舊 callback 直接丟掉）
	gen uint64
	// 「Redis 已經確認訂閱好了」的訊號：只用 close，不送值（close 一次就能叫醒所有在等的人，之後來讀的也立刻過）
	//   - 建立：addViewer 建 entry 時一起 make（mu 底下）
	//   - 換新的：worker 決定送 UNSUB 的那一刻（mu 底下）——「已確認」這句話從那一刻起就不成立了，
	//     不換的話，UNSUB 進行中進來的人會拿到舊的、早就 close 的 ready → 立刻 join_ok → 漏訊息
	//   - close：只有 confirmSubscribed（consumer 收到確認時，mu 底下）
	//   - 等：Join 在鎖外 <-ready
	ready    chan struct{}
	state    subState // actual。只有 worker、confirmSubscribed、確認 timer 會改
	failures int      // SUB 連續失敗次數，算 backoff；SUB 成功歸零
}

// desired：有沒有人要這間房（有人在看，或還在 debounce 裡）。只看 viewers / timer，跟 state（actual）無關
func (entry *roomSubscription) shouldSubscribe() bool {
	return entry.viewers > 0 || entry.timer != nil
}

// 這台 server 上每個房間的訂閱狀態（整體設計見檔案開頭）
type roomSubscriptions struct {
	mu    sync.Mutex
	rooms map[string]*roomSubscription // room public_id → 這間房的狀態；只有 worker 會刪
	// 要讓 worker 檢查的房間。用 set 不用 queue：同一間房只會有一筆 → 大小最多 = 房間數，
	// 放進去永遠不會阻塞（buffered channel 滿了會讓拿著 mu 的人卡住，又回到原點）
	dirty map[string]struct{}
	// 叫醒 worker：buffer 1，非阻塞送（已經有訊號在裡面就不用再送，worker 醒來會把 dirty 整批拿走）
	wake chan struct{}

	// 注入：正式環境是 PubSub.Subscribe / Unsubscribe（redis.go 負責把 roomID 轉成 channel 名稱）/ time.AfterFunc，test 換成假的
	// 這裡只認 roomID，不知道 Redis channel 怎麼命名
	subscribe   func(ctx context.Context, roomID string) error
	unsubscribe func(ctx context.Context, roomID string) error
	afterFunc   func(d time.Duration, f func()) stopper
	logger      *slog.Logger
}

// 建完還要另外 go run(ctx) 啟動 worker（NewRedisStore 負責），不然標了 dirty 沒人處理，Join 會一直等不到確認
func newRoomSubscriptions(
	subscribe func(ctx context.Context, roomID string) error,
	unsubscribe func(ctx context.Context, roomID string) error,
	afterFunc func(d time.Duration, f func()) stopper,
	logger *slog.Logger,
) *roomSubscriptions {
	return &roomSubscriptions{
		rooms:       make(map[string]*roomSubscription),
		dirty:       make(map[string]struct{}),
		wake:        make(chan struct{}, 1),
		subscribe:   subscribe,
		unsubscribe: unsubscribe,
		afterFunc:   afterFunc,
		logger:      logger,
	}
}

// 一條連線進入 roomID，回傳時 Redis 已經確認訂閱好了（join_ok 之後補撈歷史才補得到洞）
// 回傳 error = 等確認 timeout（SUB 失敗也是表現成等不到確認：worker 在背景 backoff 重試），
// 已經用 Leave 退回，呼叫端回 ws_join_unavailable
func (this *roomSubscriptions) Join(ctx context.Context, roomID string) error {
	// 鎖底下：改 viewers、必要時標 dirty，拿到這間房的 ready（SUBSCRIBE 由 worker 送）
	ready, err := this.addViewer(roomID)
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

// Join 在鎖底下的那一半：只改記憶體，回傳這間房的 ready 給 Join 在鎖外等
// 回傳 error 現在不會發生了（SUB 交給 worker），留著是為了不動 Join 的形狀；之後可以拿掉
func (this *roomSubscriptions) addViewer(roomID string) (<-chan struct{}, error) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		// 新房間：只建 entry、標 dirty，不送 SUB（I/O 交給 worker，不在 mu 底下做）
		// state 是 subIdle + viewers 1 → worker 的 nextAction 會決定送 SUB
		// Join 拿到的 ready 要等 worker 送出 SUB、consumer 收到確認才會 close
		newEntry := &roomSubscription{viewers: 1, ready: make(chan struct{}), state: subIdle}
		this.rooms[roomID] = newEntry
		this.markDirtyLocked(roomID)
		// 讓最後的 return entry.ready 三個分支都能用
		entry = newEntry
	} else if entry.viewers == 0 {
		// viewers == 0 但 entry 還在：debounce 中（timer != nil），
		// 或 expire 已經跑過、等 worker 送 UNSUB / 刪 entry（timer == nil）
		// Stop + 設 nil 只是省掉一次白跑（timer 觸發 → 搶鎖 → 放棄），不是正確性的來源：
		// Stop() 可能回 false（timer 已觸發、callback 在等鎖），取消不了
		// 正確性靠 expire 的 gen / viewers 檢查，不要因為這裡有 Stop 就拿掉那邊的檢查（mutation 驗過：拿掉 Stop 測試照過）
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		entry.viewers += 1
		// desired 可能從「不訂」變回「要訂」（expire 已跑過的情況）→ 一律標
		// 多標只是多跑一次 reconcile（nextAction 回 actionNone），漏標房間會卡住
		this.markDirtyLocked(roomID)
	} else {
		// 本來就有人在看：desired 沒變，不用標
		entry.viewers += 1
	}
	return entry.ready, nil
}

// consumer 收到 Redis 對 SUBSCRIBE roomID 的確認時呼叫
// 同一間房可能被呼叫多次：go-redis 斷線重連後會自動重新 SUBSCRIBE 所有記住的 channel，每個都會再回一次確認；
// 確認 timer 退回重送時也會再收到一次
//
// 不用先檢查 ready 是不是 close 過：只有 subSent → subConfirmed 這一步會 close，
// 而要從 subConfirmed 再回到 subSent 一定要先經過「worker 決定 UNSUB」（換新的 ready）→ subSent 時 ready 一定還沒 close
func (this *roomSubscriptions) confirmSubscribed(roomID string) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		// 確認晚到，entry 已經不在了（worker 已經 UNSUB 完並刪掉）：沒人在等，忽略
		return
	}
	if entry.state != subSent {
		// 不是在等確認：重複的確認（subConfirmed）、過期的確認（subIdle）→ 不理
		// subRetry 也不理：這時候不知道 SUB 到底有沒有送到，worker 等一下會重送（SUBSCRIBE 冪等），
		// 那次的確認會在 subSent 時收到。維持「只有 subSent → subConfirmed 會 close ready」這條規則，
		// 不用另外推理 retry 時收到的確認是哪一次 SUB 的；代價只是 Join 多等一次來回
		return
	}
	// 正在等：簽收 + 叫醒所有在等的 Join
	entry.state = subConfirmed
	close(entry.ready)
}

// 一條連線離開 roomID。不做 I/O：最後一個人離開只啟動 debounce timer
// 不用標 dirty：timer != nil → desired 還是「要訂」，沒變；30 秒後 expire 才會標
func (this *roomSubscriptions) Leave(ctx context.Context, roomID string) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil || entry.viewers == 0 {
		// 沒 Join 過就 Leave（呼叫端的 bug）：記 log，不讓 viewers 變負的
		this.logger.WarnContext(ctx, "leave room without join", "room", roomID)
		return
	} else if entry.viewers == 1 {
		entry.viewers = 0
		// 要記得把gen加一，以免下次關錯
		entry.gen += 1
		gen := entry.gen
		// 啟動 debounce timer，30 秒後 expire
		entry.timer =
			this.afterFunc(unsubscribeDelay, func() {
				this.expire(roomID, gen)
			})
	} else {
		entry.viewers -= 1
	}
}

// debounce 到期（gen = 這個 timer 出生時的編號）
// timer = nil → desired 變成「不訂」（viewers 已經是 0）；送 UNSUB、刪 entry 都交給 worker，這裡不做 I/O、不刪
func (this *roomSubscriptions) expire(roomID string, gen uint64) {
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
	entry.timer = nil
	this.markDirtyLocked(roomID)
}

// 在 mu 底下呼叫：標記 + 非阻塞叫醒（wake 已經有訊號就不用再送）
func (this *roomSubscriptions) markDirtyLocked(roomID string) {
	// Kotlin：dirty[roomID] = Unit（struct{}{} ≈ Unit，map 的 value 不存東西 = 當 set 用）
	this.dirty[roomID] = struct{}{}
	// Kotlin：wake.trySend(Unit)（Go 沒有 trySend，select + 空 default 是固定寫法）
	select {
	// trySend 成功：ch <- v 是「送進」，被選中時已經送完了，body 不用再做事
	case this.wake <- struct{}{}:
	// trySend 失敗（buffer 滿）：已經有一個訊號 = worker 醒來一定會看到這次標的，不用再送
	// 不能拿掉 default：沒有它就會卡在這裡等，而且是拿著 mu 卡住
	default:
	}
}
