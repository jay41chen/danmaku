package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	if err := InitDB("postgres://danmaku:danmaku@localhost:5432/danmaku"); err != nil {
		log.Fatal("db connect error:", err)
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/messages", handleMessages)

	port := "8080"
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
