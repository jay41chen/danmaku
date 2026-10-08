package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	if err := InitRedis("localhost:2907"); err != nil{
		log.Fatal("redis init error:",err)
	}
	if err := InitDB("postgres://danmaku:danmaku@localhost:3192/danmaku"); err != nil {
		log.Fatal("db init error:", err)
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/messages", handleMessages)

	port := "8080"
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
