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
func (s *RedisStore) Publish(ctx context.Context, roomID, msg string) error {
	return s.client.Publish(ctx, roomChannel(roomID), msg).Err()
}

// 委派給 roomSubscriptions.Join
func (s *RedisStore) JoinRoom(ctx context.Context, roomID string) error {
	return s.rooms.Join(ctx, roomID)
}

// 委派給 roomSubscriptions.Leave
func (s *RedisStore) LeaveRoom(ctx context.Context, roomID string) {
	s.rooms.Leave(ctx, roomID)
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
// onResync：某間房需要 client 重新撈歷史的那一刻呼叫，由呼叫端決定怎麼通知。兩種時機：
//   - 重建後重新訂閱確認（重建當下還有人在看的房間，每次重建最多一次）
//   - 已確認的房間又收到確認（reconfirm；有人在看才呼叫）。重複確認的來源：(a) go-redis 重連後重新 SUBSCRIBE 所有已知 channel；
//     (b) 確認 timer 到期、worker 重送 SUBSCRIBE；(c) Redis 斷線時 SUBSCRIBE 寫入失敗，go-redis 仍記住 channel、房間進 subRetry，
//     worker 重送時 conn() 先重新訂閱所有已知 channel 再訂閱這間房，同一間房可能收到兩次確認（第二次是 reconfirm；
//     要求重送時 go-redis 的連線仍是 nil，若 receive loop 已先重連，較早的確認在 subRetry 期間被忽略，只剩一次）。
//     都是接受的誤報（每個 viewer 多撈一次歷史）；重建之後遇到 (c)，同一間房可能一次重建路徑 resync 加一次 reconfirm resync
func (s *RedisStore) Subscribe(ctx context.Context, handler func(roomID, msg string), onResync func(roomID string)) error {
	for {
		ps := s.pubsub.Load()
		// m 是 interface{}（any）：這條 channel 會送兩種型別（*Subscription / *Message），Go 沒有 sealed class，只能用 any 裝
		// switch msg := m.(type) 是 Go 標準的 type switch（≈ Kotlin 的 when (m) { is X -> ... }）：
		// 每個 case 裡的 msg 已經是那個 case 的型別，可以直接用欄位；沒對到的型別直接略過
		for m := range ps.ChannelWithSubscriptions() {
			switch msg := m.(type) {
			case *redis.Subscription:
				// 心跳 channel 的訂閱確認：沒有房間在等，也不能走 roomIDOf（每次都會記 warning）
				if msg.Channel == s.heartbeat {
					continue
				}
				s.handleSubscription(ctx, msg, onResync)
			case *redis.Message:
				if msg.Channel == s.heartbeat {
					s.watchdog.beat()
					continue
				}
				roomID, ok := s.roomIDOf(ctx, msg.Channel)
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
		if s.pubsub.Load() == ps {
			// 不是重建造成的關閉
			return nil
		}
		s.rooms.resetAfterRebuild(time.Now())
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
// 需要 resync 的兩種情況（confirmSubscribed 回報）都會通知 onResync，onResync 在鎖外呼叫（它會去 Hub 廣播）：
//   - 重建後第一次確認、重建當下還有人在看的房間：記恢復時間
//   - reconfirm（已確認的房間又收到確認，視為 go-redis 重連、斷線期間可能漏訊息）且有人在看：記 reconfirm 次數，不記恢復時間。
//     重複確認的來源有三：(a) go-redis 重連重新 SUBSCRIBE 所有已知 channel；(b) 確認 timer 到期、worker 重送 SUBSCRIBE；
//     (c) SUBSCRIBE 寫入失敗後 worker 重送，go-redis 的 conn() 先重新訂閱所有已知 channel（含這間房）再訂一次，可能收到兩次確認（要求重送時 go-redis 的連線仍是 nil；若 receive loop 已先重連，較早的確認在 subRetry 期間被忽略，只剩一次）。
//     都是接受的誤報；重建之後遇到 (c)，同一間房可能一次重建路徑 resync 加一次 reconfirm resync
func (s *RedisStore) handleSubscription(ctx context.Context, msg *redis.Subscription, onResync func(roomID string)) {
	if msg.Kind != "subscribe" {
		return
	}
	roomID, ok := s.roomIDOf(ctx, msg.Channel)
	if !ok {
		return
	}
	result := s.rooms.confirmSubscribed(roomID)
	if result.reconfirm {
		// 已確認的房間又收到確認：go-redis 重連（或確認 timer 重送）。不記恢復時間，那是重建路徑的指標
		prom.PubSubReconfirmed()
	} else if result.resync {
		prom.PubSubResubscribed(time.Since(result.since))
	}
	if result.resync {
		onResync(roomID)
	}
}

// roomChannel 的反向：去掉 "room:" 前綴拿回 roomID；不是房間的 channel 記 log、回 false 讓呼叫端跳過
// 回傳 ok 而不是自己 continue / return：要不要跳過交給呼叫端決定
func (s *RedisStore) roomIDOf(ctx context.Context, channel string) (string, bool) {
	roomID, ok := strings.CutPrefix(channel, roomChannel(""))
	if !ok {
		s.logger.WarnContext(ctx, "unexpected channel, skipped", "channel", channel)
	}
	return roomID, ok
}
