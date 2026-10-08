package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	clients = make(map[*websocket.Conn]bool)
	mutex   sync.Mutex
)

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade error:", err)
		return
	}
	defer conn.Close()

	mutex.Lock()
	clients[conn] = true
	mutex.Unlock()

	log.Println("new client connected, total:", len(clients))

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			mutex.Lock()
			delete(clients, conn)
			mutex.Unlock()
			log.Println("client disconnected, total:", len(clients))
			break
		}
		log.Printf("received: %s", msg)

		if err := SaveMessage(string(msg)); err != nil {
			log.Println("save error:", err)
		}

		if err := Publish(string(msg)); err != nil {
			log.Println("publish error:", err)
		}
	}
}

func StartBroadcast() {
	Subscribe(func(msg string) {
		mutex.Lock()
		for client := range clients {
			if err := client.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				client.Close()
				delete(clients, client)
			}
		}
		mutex.Unlock()
	})
}
