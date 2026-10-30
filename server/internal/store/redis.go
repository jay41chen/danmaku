package store

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStoreInterface interface {
	Publish(ctx context.Context, roomID, msg string) error // 發到 roomChannel(roomID)
	Subscribe(ctx context.Context, handler func(roomID, msg string)) error
	JoinRoom(ctx context.Context, roomID string) error // 委派給 roomSubscriptions.Join
	LeaveRoom(ctx context.Context, roomID string)      // 委派給 roomSubscriptions.Leave
	CheckHealth(ctx context.Context) error
}

// Redis channel 名稱：Publish、Subscribe 收訊息、roomSubscriptions 的 SUBSCRIBE / UNSUBSCRIBE 都經過這裡，不要各自拼字串
// 只有 redis.go 知道命名規則；roomSubscriptions 只認 roomID
func roomChannel(roomID string) string {
	return "room:" + roomID
}

type RedisStore struct {
	client *redis.Client
	// 整台 server 只有一個長期存活的 PubSub，建立時不訂任何 channel，房間的 channel 之後動態 SUBSCRIBE / UNSUBSCRIBE
	pubsub *redis.PubSub
	rooms  *roomSubscriptions
	logger *slog.Logger
}

// constuctor
func NewRedisStore(ctx context.Context, address string) (*RedisStore, error) {
	logger := slog.Default().With("component", "redis")

	option, err := redis.ParseURL(address)
	if err != nil {
		// 不 fallback 成 Options{Addr: address}：URL 寫錯時整串（含密碼）會變成連線位址，被印進 log
		// ParseURL 自己的錯誤訊息也會帶出 URL，所以回固定訊息
		return nil, errors.New("invalid redis url: want redis:// or rediss://[[user]:password@]host[:port][/db-number], special characters in password must be percent-encoded")
	}

	client := redis.NewClient(option)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	logger.Info("connected")

	pubsub := client.Subscribe(ctx)
	rooms := newRoomSubscriptions(
		func(ctx context.Context, roomID string) error { return pubsub.Subscribe(ctx, roomChannel(roomID)) },
		func(ctx context.Context, roomID string) error { return pubsub.Unsubscribe(ctx, roomChannel(roomID)) },
		func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		logger,
	)

	return &RedisStore{client: client,
		pubsub: pubsub,
		rooms:  rooms,
		logger: logger,
	}, nil
}

func (this *RedisStore) CheckHealth(ctx context.Context) error {
	return this.client.Ping(ctx).Err()
}

func (this *RedisStore) Publish(ctx context.Context, roomID, msg string) error {
	return this.client.Publish(ctx, roomChannel(roomID), msg).Err()
}

func (this *RedisStore) JoinRoom(ctx context.Context, roomID string) error {
	return this.rooms.Join(ctx, roomID)
}

func (this *RedisStore) LeaveRoom(ctx context.Context, roomID string) {
	this.rooms.Leave(ctx, roomID)
}

// 用 ChannelWithSubscriptions 不用 Channel()：Channel() 會把 Redis 對 SUBSCRIBE 的確認（*Subscription）丟掉
// （go-redis v9.23 pubsub.go initMsgChan），我們要靠這個確認告訴 roomSubscriptions「訂閱好了」
// 兩者都會丟掉 *Pong，所以不能改用 PING 當確認（go-redis 自己每 3 秒也會 PING 做健康檢查）
//
// 不做外層 retry / backoff：ChannelWithSubscriptions 只有在 PubSub 被 Close 時才會關閉
// （pubsub.go initAllChan：只有遇到 pool.ErrClosed 才 close allCh）；
// 網路斷線時 go-redis 會自己重連，並重新訂閱它記住的 channel（每個都會再回一次確認，confirmSubscribed 要能重複呼叫）。
// 如果在這裡另外建一個新的 PubSub，所有動態訂閱的房間都會遺失
//
// 已知問題（既有行為）：channel 的 buffer 滿了（我們消費太慢）時，
// go-redis 會直接丟掉那則訊息、只記 log（pubsub.go initAllChan 約 840-850）
func (this *RedisStore) Subscribe(ctx context.Context, handler func(roomID, msg string)) error {
	// m 是 interface{}（any）：這條 channel 會送兩種型別（*Subscription / *Message），Go 沒有 sealed class，只能用 any 裝
	// switch msg := m.(type) 是 Go 標準的 type switch（≈ Kotlin 的 when (m) { is X -> ... }）：
	// 每個 case 裡的 msg 已經是那個 case 的型別，可以直接用欄位；沒對到的型別直接略過
	for m := range this.pubsub.ChannelWithSubscriptions() {
		switch msg := m.(type) {
		case *redis.Subscription:
			this.handleSubscription(ctx, msg)
		case *redis.Message:
			roomID, ok := this.roomIDOf(ctx, msg.Channel)
			if !ok {
				continue
			}
			handler(roomID, msg.Payload)
		}
	}
	return nil
}

// Redis 對 SUBSCRIBE / UNSUBSCRIBE 的確認：只有 subscribe 的確認拿去叫醒在 Join 裡等的人
// 抽出來是為了 test 能直接餵假的 *redis.Subscription（consumer 迴圈讀的是 go-redis 的 channel，fake 不進去）
//
// 一定要過濾 Kind，unsubscribe 的確認不能拿去 close ready：
//
//	t0  expire：寫出 UNSUBSCRIBE room:a、刪 entry、放鎖
//	t1  X 進房：建新 entry（新的 ready）、寫出 SUBSCRIBE room:a、放鎖、開始等
//	t2  t0 那個 UNSUBSCRIBE 的確認才到 → 如果不過濾，會 close X 的新 ready
//	t3  X 拿到 join_ok，但它的 SUBSCRIBE 還沒被確認 → 又回到「join_ok 之後補撈歷史也補不到」的洞
func (this *RedisStore) handleSubscription(ctx context.Context, msg *redis.Subscription) {
	if msg.Kind != "subscribe" {
		return
	}
	roomID, ok := this.roomIDOf(ctx, msg.Channel)
	if !ok {
		return
	}
	this.rooms.confirmSubscribed(roomID)
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
