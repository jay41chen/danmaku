package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/jay41chen/danmaku/server/internal/handler"
	"github.com/jay41chen/danmaku/server/internal/store"
)

type ApplicationStore struct {
	dbStore          *store.DBStore
	redisStore       *store.RedisStore
	apiHandler       *handler.APIHandler
	websocketHandler *handler.WebsocketHandler
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
	dbStore, err := store.NewDBStore(context.Background(), dbURL)
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
	redisStore, err := store.NewRedisStore(context.Background(), redisURL)
	if err != nil {
		log.Fatal("redis init error", err)
	}
	this.redisStore = redisStore
}

func (this *ApplicationStore) initAPIHandler() {
	this.apiHandler = handler.NewAPIHandler(this.dbStore, this.redisStore)
}

func (this *ApplicationStore) initWebsocketHandler() {
	this.websocketHandler = handler.NewWebsocketHandler(this.redisStore, this.dbStore)
}

func (this *ApplicationStore) registerAPIRoute() {
	http.HandleFunc("/messages", this.apiHandler.HandleMessages)
	http.HandleFunc("/healthcheck", this.apiHandler.CheckServerHealth)
}

func (this *ApplicationStore) registerWSRoute() {
	http.HandleFunc("/ws", this.websocketHandler.HandleWebsocket)
}

func startServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
