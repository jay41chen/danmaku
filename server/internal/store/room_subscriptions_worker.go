package store

import (
	"context"
	"time"
)

// roomSubscriptions 的 worker 端：整台 server 一個 goroutine，負責所有 SUB / UNSUB（為什麼這樣設計見 room_subscriptions.go 開頭）
//
// 流程：
//   - run：被 wake 叫醒 → 把 dirty 整包換走 → 一間一間 reconcile
//   - reconcile：decide（鎖底下，nextAction 比對 desired / actual）→ send（鎖外 I/O）→ record（鎖底下記結果、再標一次 dirty）
//   - 兩種 timer 在 worker 外面把房間重新標 dirty，讓 worker 再看一次：
//     SUB 失敗 → backoff 到期（onSubscribeFailed）；SUB 送出但等不到確認 → 確認逾時（confirmTimeout）

const (
	// worker 送每個 SUB / UNSUB 時自己建的 ctx 上限（執行當下才建，排隊的時間不會吃掉它）
	// 不能再短：寫入一 timeout，go-redis 會判定連線壞掉、整條 PubSub 拆掉重連，所有房間都漏訊息
	// 卡住的只有 worker 自己，Join 有自己的 200ms（ws.go），不受這個值影響
	redisCommandTimeout = 500 * time.Millisecond
	// SUB 送出後多久還沒收到確認，就當作沒訂、讓 worker 重送（SUBSCRIBE 是冪等的，Redis 會再回一次確認）
	// 確認可能被延遲或丟掉：consumer 卡在廣播慢 client（B3）、go-redis 的 channel 滿了直接丟
	confirmResendDelay = 500 * time.Millisecond
	// SUB 失敗的 backoff：retryBaseDelay × 2^(failures-1)，最多 retryMaxDelay
	// Redis 掛掉時不會每 100ms 送一次、寫一行 log
	retryBaseDelay = 100 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
)

// reconcile 在鎖底下做的決定，帶到鎖外執行（鎖底下只決定，I/O 在鎖外）
// 跟 subState 分開一個 const 區塊：iota 是「在這個 const 區塊裡第幾行」，同一塊的話 actionNone 不會是 0
type reconcileAction int

const (
	actionNone        reconcileAction = iota // 沒事：desired 和 actual 一致（零值）
	actionSubscribe                          // 送 SUBSCRIBE
	actionUnsubscribe                        // 送 UNSUBSCRIBE
	actionDelete                             // desired、actual 都是「沒訂」：刪 entry（不用 I/O）
)

// 由 NewRedisStore 用 go 啟動，ctx 結束就 return
func (this *roomSubscriptions) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-this.wake:
			// 拿鎖把 dirty 整包拿走、換一個新的空 map，然後放鎖，在鎖外一間一間 reconcile
			//   - 不能拿著鎖 range：reconcile 要送 Redis 指令，而且它自己也要拿 mu（不能重入）
			//   - 一定要換新的：不換的話鎖外讀、鎖內寫的是同一份 map → fatal error: concurrent map iteration and map write
			//   - reconcile 期間別人新標的房寫進新的 map，wake 也會再有訊號 → 下一輪處理，不會漏
			this.mu.Lock()
			dirties := this.dirty
			this.dirty = make(map[string]struct{})
			this.mu.Unlock()
			for roomID := range dirties {
				this.reconcile(ctx, roomID)
			}
		}
	}
}

// 一間房：鎖底下決定 → 鎖外 I/O → 鎖底下記結果
// 三段各自拿 / 放鎖：I/O 那段不拿鎖，Redis 卡住時 Join / Leave 照常
func (this *roomSubscriptions) reconcile(ctx context.Context, roomID string) {
	action := this.decide(roomID)
	if action != actionSubscribe && action != actionUnsubscribe {
		// 沒事，或 entry 已經在 decide 裡刪掉了
		return
	}
	err := this.send(ctx, action, roomID)
	this.record(ctx, action, roomID, err)
}

// 第一段（鎖底下）：比對 desired / actual 決定要做什麼；要刪 entry 的話直接在這裡刪
// 「讀狀態、做決定、改 state / ready」要在同一次拿鎖裡完成：中間放鎖的話，別人插進來改了，決定就是錯的
func (this *roomSubscriptions) decide(roomID string) reconcileAction {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID]
	if entry == nil {
		return actionNone
	}
	action := entry.nextAction()
	if action == actionDelete {
		// 唯一刪 entry 的地方
		delete(this.rooms, roomID)
	}
	return action
}

// 第二段（鎖外）：送指令
// 只用區域變數和建構後就不會變的函式欄位，不碰 entry 等共用資料 → 不需要 mu
// 之後要在這裡加 log 的話，不要讀 entry 的欄位（那就變成鎖外讀共用資料了）
func (this *roomSubscriptions) send(ctx context.Context, action reconcileAction, roomID string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, redisCommandTimeout)
	defer cancel() // 釋放 WithTimeout 的計時器；漏掉的話 go vet（lostcancel）會報
	if action == actionSubscribe {
		return this.subscribe(cmdCtx, roomID)
	}
	return this.unsubscribe(cmdCtx, roomID)
}

// 第三段（鎖底下）：依結果更新 entry
func (this *roomSubscriptions) record(ctx context.Context, action reconcileAction, roomID string, err error) {
	this.mu.Lock()
	defer this.mu.Unlock()
	entry := this.rooms[roomID] // 重新讀：I/O 期間 entry 可能被改過（不會是 nil：只有 worker 會刪，而 worker 就是自己）

	switch {
	case action == actionSubscribe && err != nil:
		this.onSubscribeFailed(ctx, entry, roomID, err)
		// 不走到最後的 markDirtyLocked：不然 worker 立刻重試，backoff 就沒意義了
		return
	case action == actionSubscribe:
		this.onSubscribeSent(entry, roomID)
	case err != nil:
		// UNSUB 失敗：只記 log，不重試、不改 state（決定 UNSUB 時已經是 subIdle）
		// go-redis 的 Unsubscribe 先從它記的 channel 刪掉才寫指令：
		// 寫入失敗 = 連線壞了 → 重連只會重訂剩下的 → 實際上已經不訂了
		this.logger.WarnContext(ctx, "unsubscribe room failed", "room", roomID, "error", err)
	}
	// I/O 期間 desired 可能變了（TOCTOU）：再標一次，下一輪 nextAction 重新對答案
	// 不會無限迴圈：下一輪如果 desired / actual 已經一致，nextAction 回 actionNone，不會再標
	this.markDirtyLocked(roomID)
}

// 在 mu 底下呼叫。SUB 沒送出去：state 改成 subRetry，backoff 之後再標 dirty 重試
// 不能退回 subIdle：go-redis 的 Subscribe 寫入失敗也會把 channel 記下來（pubsub.go 約 232-244），重連時會自己重訂
// → 實際上「可能有訂」。退回 subIdle 的話，這時候大家都走了 nextAction 會直接刪 entry、不送 UNSUB → 這台永遠訂著
func (this *roomSubscriptions) onSubscribeFailed(ctx context.Context, entry *roomSubscription, roomID string, err error) {
	// 不改的話要白等確認 timer 才會重送
	// 只在還是 subSent 時改：寫入回報失敗但 Redis 其實收到了、確認已經先到（subConfirmed）的話不要蓋掉
	if entry.state == subSent {
		entry.state = subRetry
	}
	entry.failures++
	backoff := retryBackoff(entry.failures)
	this.logger.WarnContext(ctx, "subscribe room failed, will retry",
		"room", roomID, "failures", entry.failures, "backoff", backoff, "error", err)
	// timer 的 callback 在另一個 goroutine 跑、手上沒有鎖 → 自己拿鎖再 markDirtyLocked
	this.afterFunc(backoff, func() {
		this.mu.Lock()
		defer this.mu.Unlock()
		this.markDirtyLocked(roomID)
	})
}

// 在 mu 底下呼叫。SUB 寫出去了（還沒確認）：失敗次數歸零，開確認 timer
// 確認 timer 不存起來、不 Stop：時間到自己檢查還要不要重送，過期的也無害（SUBSCRIBE 是冪等的）
// 不能存進 entry.timer：那是 debounce timer，nextAction 靠它算 desired，存進去這間房就永遠不會 UNSUB
func (this *roomSubscriptions) onSubscribeSent(entry *roomSubscription, roomID string) {
	entry.failures = 0
	this.afterFunc(confirmResendDelay, func() { this.confirmTimeout(roomID) })
}

// 確認 timer 到期：還在等確認（subSent）就退回 subRetry、標 dirty，讓 worker 重送 SUB
// 其他情況（已確認、已決定 UNSUB、entry 不在）→ 什麼都不做
func (this *roomSubscriptions) confirmTimeout(roomID string) {
	// timer 觸發：在另一個 goroutine 執行，要自己拿鎖
	this.mu.Lock()
	defer this.mu.Unlock()
	// 重新從 map 拿：500ms 後 entry 可能已經被刪掉、甚至換成新的
	entry := this.rooms[roomID]
	if entry == nil || entry.state != subSent {
		return
	}
	entry.state = subRetry
	this.markDirtyLocked(roomID)
}

// 在 mu 底下呼叫：比對 desired / actual，決定 worker 要做什麼
// 要送指令的話，在送出之前（還在鎖底下）就先改好 state / ready：
//   - SUB → 先改 subSent：確認可能比 subscribe 回來還早到，confirmSubscribed 只接受 subSent，晚改的話會被丟掉、白等確認 timer
//   - UNSUB → 先換新的 ready、退回 subIdle：送 UNSUB 期間有人 Join，要拿到新的 ready 等新的確認
func (entry *roomSubscription) nextAction() reconcileAction {
	shouldSubscribe := entry.shouldSubscribe()

	switch {
	// desired（shouldSubscribe，看人）× actual（state，看 Redis）：
	//             該訂        不該訂
	//   idle      SUB         刪
	//   retry     SUB         UNSUB（go-redis 可能記著這個 channel）
	//   sent      沒事        UNSUB
	//   confirmed 沒事        UNSUB
	// retry 在「能不能送 SUB」跟 idle 同一組，在「可能有訂」跟 sent / confirmed 同一組 → 第 1 格和第 2、3 格分組不一樣
	case shouldSubscribe && (entry.state == subIdle || entry.state == subRetry):
		// 該訂但還沒送（新房間、SUB 失敗 backoff 到期、確認等太久、UNSUB 送到一半有人回來）
		entry.state = subSent
		return actionSubscribe
	case !shouldSubscribe && entry.state != subIdle:
		// 不該訂了，但 Redis 那邊還訂著、或可能訂著（SUB 已送出 / 送失敗 / 確認逾時）
		entry.ready = make(chan struct{})
		entry.state = subIdle
		return actionUnsubscribe
	case !shouldSubscribe && entry.state == subIdle:
		// 不該訂、Redis 那邊也沒訂（或 UNSUB 已經送了）：entry 沒東西要記
		return actionDelete
	default:
		// 該訂，而且 SUB 已送出（等確認中）或已確認
		return actionNone
	}
}

// SUB 第 failures 次連續失敗後要等多久：retryBaseDelay × 2^(failures-1)，最多 retryMaxDelay
// 用迴圈乘不用 retryBaseDelay << (failures-1)：failures 很大時位移會溢位（time.Duration 是 int64），變成負數或 0 → 立刻重試
// 到上限就停，迴圈次數不會超過「翻幾倍到上限」
func retryBackoff(failures int) time.Duration {
	backoff := retryBaseDelay
	for i := 1; i < failures && backoff < retryMaxDelay; i++ {
		backoff *= 2
	}
	return min(backoff, retryMaxDelay)
}
