package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"context"
)

type ApplicationStore struct {
	dbStore *DBStore
	redisStore *RedisStore
	apiHandler *APIHandler
	websocketHandler *WebsocketHandler
}

func main() {
	// generate application store to keep singleton instances
	newApplicationStore()

	startServer()
}

func newApplicationStore() *ApplicationStore {
	appStore := ApplicationStore{}
	appStore.initDB()
	appStore.initRedis()
	appStore.initAPIHandler()
	appStore.registerAPIRoute()
	appStore.initWebsocketHandler()
	go appStore.websocketHandler.StartBroadcast()
	appStore.registerWSRoute()
	return &appStore
}

func (this *ApplicationStore) initDB() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	}
	dbStore, err := NewDBStore(context.Background(), dbURL)
	if err != nil {
		// fatal will terminate the execution!!
		log.Fatal("db init error:", err) 
	}
	this.dbStore = dbStore
}

func (this *ApplicationStore) initRedis() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "localhost:2907"
	}
	redisStore, err := NewRedisStore(context.Background(), redisURL)
	if err != nil {
		log.Fatal("redis init error", err)
	}
	this.redisStore = redisStore
}

func (this *ApplicationStore) initAPIHandler() {
	this.apiHandler = NewAPIHandler(this.dbStore, this.redisStore,)
}

func (this *ApplicationStore) initWebsocketHandler() {
	this.websocketHandler = NewWebsocketHandler(this.redisStore, this.dbStore)
}

func (this *ApplicationStore) registerAPIRoute() {
	http.HandleFunc("/messages", this.apiHandler.handleMessages)
	http.HandleFunc("/healthcheck", this.apiHandler.checkServerHealth)
}

func (this *ApplicationStore) registerWSRoute() {
	http.HandleFunc("/ws", this.websocketHandler.handleWebsocket)
}

func startServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}