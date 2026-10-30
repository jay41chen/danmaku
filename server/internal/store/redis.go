package store

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"
)

type RedisStoreInterface interface {
	Publish(ctx context.Context, roomID, msg string) error // 發到 roomChannel(roomID)
	Subscribe(ctx context.Context, handler func(roomID, msg string)) error
	JoinRoom(ctx context.Context, roomID string) error // 委派給 roomSubscriptions.Join
	LeaveRoom(ctx context.Context, roomID string)      // 委派給 roomSubscriptions.Leave
	CheckHealth(ctx context.Context) error
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
		func(ctx context.Context, ch string) error { return pubsub.Subscribe(ctx, ch) },
		func(ctx context.Context, ch string) error { return pubsub.Unsubscribe(ctx, ch) },
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

// 不做外層 retry / backoff：go-redis 的 Channel() 只有在 PubSub 被 Close 時才會關閉
// （go-redis v9.23 pubsub.go initMsgChan：只有遇到 pool.ErrClosed 才 close msgCh）；
// 網路斷線時 go-redis 會自己重連，並重新訂閱它記住的 channel。
// 如果在這裡另外建一個新的 PubSub，所有動態訂閱的房間都會遺失
//
// 已知問題（既有行為）：Channel() 的 buffer 滿了（我們消費太慢）時，
// go-redis 會直接丟掉那則訊息、只記 log（pubsub.go 約 787-796）
func (this *RedisStore) Subscribe(ctx context.Context, handler func(roomID, msg string)) error {
	for msg := range this.pubsub.Channel() {
		roomID, ok := strings.CutPrefix(msg.Channel, roomChannel("")) // 去掉 "room:" 前綴
		if !ok {
			this.logger.WarnContext(ctx, "unexpected channel, skipped", "channel", msg.Channel)
			continue
		}
		handler(roomID, msg.Payload)
	}
	return nil
}
