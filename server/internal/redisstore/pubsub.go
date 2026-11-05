package redisstore

import (
	"context"
	"strings"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/redis/go-redis/v9"
)

// Redis channel 名稱：Publish、Subscribe 收訊息、roomSubscriptions 的 SUBSCRIBE / UNSUBSCRIBE 都經過這裡，不要各自拼字串
// 只有 pubsub.go 知道命名規則；roomSubscriptions 只認 roomID
func roomChannel(roomID string) string {
	return "room:" + roomID
}

// 每台 server 自己的心跳 channel 名稱：watchdog 發、consumer 收，不經過 roomIDOf（不是房間的 channel）
// id 是啟動時的隨機值；跟 roomChannel 一樣，只有這個檔案知道命名規則
func heartbeatChannel(id string) string {
	return "heartbeat:" + id
}

// 發到 roomChannel(roomID)
func (this *RedisStore) Publish(ctx context.Context, roomID, msg string) error {
	return this.client.Publish(ctx, roomChannel(roomID), msg).Err()
}

// 委派給 roomSubscriptions.Join
func (this *RedisStore) JoinRoom(ctx context.Context, roomID string) error {
	return this.rooms.Join(ctx, roomID)
}

// 委派給 roomSubscriptions.Leave
func (this *RedisStore) LeaveRoom(ctx context.Context, roomID string) {
	this.rooms.Leave(ctx, roomID)
}

// 用 ChannelWithSubscriptions 不用 Channel()：Channel() 會把 Redis 對 SUBSCRIBE 的確認（*Subscription）丟掉
// （go-redis v9.23 pubsub.go initMsgChan），我們要靠這個確認告訴 roomSubscriptions「訂閱好了」
// 兩者都會丟掉 *Pong，所以不能改用 PING 當確認（go-redis 自己每 3 秒也會 PING 做健康檢查）
//
// 外層迴圈只在 PubSub 被換掉時才轉一圈：
//   - 網路斷線：go-redis 自己重連，並重新訂閱它記住的 channel（每個都會再回一次確認，confirmSubscribed 要能重複呼叫）。
//     這種情況 ChannelWithSubscriptions 不會關，我們什麼都不用做
//   - 連線沒斷、但再也收不到東西（聾掉）：go-redis 不會發現。watchdog 發現自己的心跳收不到，呼叫 rebuildPubSub
//     換一個新的 PubSub、Close 舊的 → 舊 channel 關閉 → 這裡重設房間狀態、改消費新的 channel
//   - 其他原因讓 channel 關閉（有人直接 Close 目前這個 PubSub，例如 test cleanup）：指標沒變，直接 return
//
// 重設（resetAfterRebuild）一定要由 consumer 在「舊 channel 收完」之後才做，不能由 rebuildPubSub 做：
// 舊 channel 的 buffer 裡可能還殘留 subscribe 確認（舊連線的）。如果先重設、這些殘留確認才被處理，
// 會被當成新連線的確認（state 已經是 subSent）→ 提早 close ready → client 拿到 join_ok，其實新連線還沒訂好 → 漏訊息
// 在這裡重設的話，舊 channel 一定已經 range 完，之後收到的確認都來自新的 PubSub
//
// 已知問題（既有行為）：channel 的 buffer 滿了（我們消費太慢）時，
// go-redis 會直接丟掉那則訊息、只記 log（go-redis 的 pubsub.go initAllChan 約 840-850）
//
// onResync：重建後某間房重新訂閱確認的那一刻呼叫（重建當下還有人在看的房間，每次重建最多一次），由呼叫端決定怎麼通知 client 重新撈歷史
func (this *RedisStore) Subscribe(ctx context.Context, handler func(roomID, msg string), onResync func(roomID string)) error {
	for {
		ps := this.pubsub.Load()
		// m 是 interface{}（any）：這條 channel 會送兩種型別（*Subscription / *Message），Go 沒有 sealed class，只能用 any 裝
		// switch msg := m.(type) 是 Go 標準的 type switch（≈ Kotlin 的 when (m) { is X -> ... }）：
		// 每個 case 裡的 msg 已經是那個 case 的型別，可以直接用欄位；沒對到的型別直接略過
		for m := range ps.ChannelWithSubscriptions() {
			switch msg := m.(type) {
			case *redis.Subscription:
				// 心跳 channel 的訂閱確認：沒有房間在等，也不能走 roomIDOf（每次都會記 warning）
				if msg.Channel == this.heartbeat {
					continue
				}
				this.handleSubscription(ctx, msg, onResync)
			case *redis.Message:
				if msg.Channel == this.heartbeat {
					this.watchdog.beat()
					continue
				}
				roomID, ok := this.roomIDOf(ctx, msg.Channel)
				if !ok {
					continue
				}
				handler(roomID, msg.Payload)
			}
		}
		// 舊 channel 收完了才會走到這裡
		if ctx.Err() != nil {
			return nil
		}
		if this.pubsub.Load() == ps {
			// 不是重建造成的關閉
			return nil
		}
		this.rooms.resetAfterRebuild(time.Now())
	}
}

// Redis 對 SUBSCRIBE / UNSUBSCRIBE 的確認：只有 subscribe 的確認拿去叫醒在 Join 裡等的人
// 抽出來是為了 test 能直接餵假的 *redis.Subscription（consumer 迴圈讀的是 go-redis 的 channel，fake 不進去）
//
// 一定要過濾 Kind，unsubscribe 的確認不能拿去 close ready（confirmSubscribed 只看 state，分不出是哪一種確認）：
//
//	t0  worker：決定 UNSUB（換新的 ready、state=subIdle），寫出 UNSUBSCRIBE room:a
//	t1  X 進房 → worker 決定 SUB（state=subSent），寫出 SUBSCRIBE room:a，X 開始等
//	t2  t0 那個 UNSUBSCRIBE 的確認才到 → 如果不過濾，state 剛好是 subSent，會 close X 的新 ready
//	t3  X 拿到 join_ok，但它的 SUBSCRIBE 還沒被確認 → 又回到「join_ok 之後補撈歷史也補不到」的洞
//
// 重建當下有人在看、重建後第一次確認的房間（confirmSubscribed 回報需要 resync）：記恢復時間、通知 onResync。onResync 在鎖外呼叫（它會去 Hub 廣播）
func (this *RedisStore) handleSubscription(ctx context.Context, msg *redis.Subscription, onResync func(roomID string)) {
	if msg.Kind != "subscribe" {
		return
	}
	roomID, ok := this.roomIDOf(ctx, msg.Channel)
	if !ok {
		return
	}
	if resync, since := this.rooms.confirmSubscribed(roomID); resync {
		prom.PubSubResubscribeDuration.Observe(time.Since(since).Seconds())
		onResync(roomID)
	}
}

// roomChannel 的反向：去掉 "room:" 前綴拿回 roomID；不是房間的 channel 記 log、回 false 讓呼叫端跳過
// 回傳 ok 而不是自己 continue / return：要不要跳過交給呼叫端決定
func (this *RedisStore) roomIDOf(ctx context.Context, channel string) (string, bool) {
	roomID, ok := strings.CutPrefix(channel, roomChannel(""))
	if !ok {
		this.logger.WarnContext(ctx, "unexpected channel, skipped", "channel", channel)
	}
	return roomID, ok
}
