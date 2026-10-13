package store 

import (
	"context"
	"log"
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
}

// constuctor
func NewRedisStore(ctx context.Context, address string) (*RedisStore, error) {
	option, err := redis.ParseURL(address)
	if err != nil {
		option = &redis.Options { Addr: address }
	}

	client := redis.NewClient(option)
	
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	log.Println("redis connected")
	return &RedisStore{ client: client}, nil
}

func (this *RedisStore) CheckHealth(ctx context.Context) error {
	return this.client.Ping(ctx).Err()
}

func (this *RedisStore) Publish(ctx context.Context, msg string) error{
	return this.client.Publish(ctx, channel, msg).Err()
}

func (this *RedisStore) Subscribe(ctx context.Context, handler func(string)) error{
	for {
		sub := this.client.Subscribe(ctx, channel)
		ch := sub.Channel()
		for msg := range ch {
			handler(msg.Payload)
		}
		sub.Close()
		log.Println("redis subscription lost, reconnecting in 1s...")
		time.Sleep(time.Second)	
	}
}
