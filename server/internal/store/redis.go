package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const channel = "danmaku:messages"

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
		option = &redis.Options{Addr: address}
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
	for {
		sub := this.client.Subscribe(ctx, channel)
		ch := sub.Channel()
		for msg := range ch {
			handler(msg.Payload)
		}
		sub.Close()
		this.logger.Warn("redis subscription lost, retry in 1 second")
		time.Sleep(time.Second)
	}
}
