package main

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const channel = "danmaku:messages"

var redisClient *redis.Client

func InitRedis(addr string) error {
	opt, err := redis.ParseURL(addr)
	if err != nil {
		opt = &redis.Options{Addr: addr}
	}

	redisClient = redis.NewClient(opt)

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		return err
	}

	log.Println("redis connected")
	return nil
}

func CheckRedisHealth(ctx context.Context) error {
	return redisClient.Ping(ctx).Err()
}

func Publish(msg string) error {
	return redisClient.Publish(context.Background(), channel, msg).Err()
}

func Subscribe(handler func(string)) {
	for {
		sub := redisClient.Subscribe(context.Background(), channel)
		ch := sub.Channel()
		for msg := range ch {
			handler(msg.Payload)
		}
		sub.Close()
		log.Println("redis subscription lost, reconnecting in 1s...")
		time.Sleep(time.Second)
	}
}
