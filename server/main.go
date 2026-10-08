package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "localhost:2907"
	}
	if err := InitRedis(redisURL); err != nil {
		log.Fatal("redis init error:", err)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	}
	if err := InitDB(dbURL); err != nil {
		log.Fatal("db init error:", err)
	}

	go StartBroadcast()

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/messages", handleMessages)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
