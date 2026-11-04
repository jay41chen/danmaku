package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

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
	// 送 SUB / UNSUB 的 worker：整台 server 一個，跟 process 活一樣久（main 傳進來的是 context.Background()）
	// 不能等到 Subscribe 才啟動：Subscribe 是 ws.go 另外起的 goroutine，順序不保證，
	// 第一個 Join 可能比它早到，標了 dirty 卻沒人處理
	go rooms.run(ctx)

	return &RedisStore{client: client,
		pubsub: pubsub,
		rooms:  rooms,
		logger: logger,
	}, nil
}

func (this *RedisStore) CheckHealth(ctx context.Context) error {
	return this.client.Ping(ctx).Err()
}
