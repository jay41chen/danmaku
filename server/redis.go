package main

import(
	"context"
	"log"

	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

func InitRedis(addr string) error{
	rdb = redis.NewClient(
		&redis.Options{
			Addr:addr,
		},
	)

	if err := rdb.Ping(context.Background()).Err(); err != nil {
            return err
    }

    log.Println("redis connected")
    return nil
}