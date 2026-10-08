package main

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

func InitRedis(addr string) error {
	opt, err := redis.ParseURL(addr)
	if err != nil {
		opt = &redis.Options{Addr: addr}
	}

	rdb = redis.NewClient(opt)

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return err
	}

	log.Println("redis connected")
	return nil
}

const channel = "danmaku:messages"

func Publish(msg string) error {
	return rdb.Publish(context.Background(), channel, msg).Err()
}

func Subscribe(handler func(string)) {
	for {
		sub := rdb.Subscribe(context.Background(), channel)
		ch := sub.Channel()
		for msg := range ch {
			handler(msg.Payload)
		}
		sub.Close()
		log.Println("redis subscription lost, reconnecting in 1s...")
		time.Sleep(time.Second)
	}
}
