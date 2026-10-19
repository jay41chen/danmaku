package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	channel       = "danmaku:messages"
	retryMaxDelay = 30 * time.Second
)

type RedisStoreInterface interface {
	Publish(ctx context.Context, msg string) error
	Subscribe(ctx context.Context, handler func(string)) error
	CheckHealth(ctx context.Context) error
}

type RedisStore struct {
	client *redis.Client
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

	return &RedisStore{client: client,
		logger: logger,
	}, nil
}

func (this *RedisStore) CheckHealth(ctx context.Context) error {
	return this.client.Ping(ctx).Err()
}

func (this *RedisStore) Publish(ctx context.Context, msg string) error {
	return this.client.Publish(ctx, channel, msg).Err()
}

func (this *RedisStore) Subscribe(ctx context.Context, handler func(string)) error {
	var delayTime time.Duration
	for {
		subscription := this.client.Subscribe(ctx, channel)
		// 成功連上redis，重設delayTime
		delayTime = 1 * time.Second
		ch := subscription.Channel()
		for msg := range ch {
			handler(msg.Payload)
		}
		subscription.Close()
		this.logger.WarnContext(ctx, "redis subscription lost", "retryIn", delayTime)
		time.Sleep(delayTime)
		// backoff retry
		delayTime *= 2
		if delayTime >= retryMaxDelay {
			delayTime = retryMaxDelay
		}
	}
}
