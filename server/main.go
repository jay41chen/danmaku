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
	apiHandler *APIHandler
}

func main() {
	// generate application store to keep singleton instances
	newApplicationStore()

	// redisURL := os.Getenv("REDIS_URL")
	// if redisURL == "" {
	// 	redisURL = "localhost:2907"
	// }
	// if err := InitRedis(redisURL); err != nil {
	// 	log.Fatal("redis init error:", err)
	// }

	// dbURL := os.Getenv("DATABASE_URL")
	// if dbURL == "" {
	// 	dbURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	// }

	// if err := InitDB(dbURL); err != nil {
	// 	log.Fatal("db init error:", err)
	// }

	
	// go StartBroadcast()
	// http.HandleFunc("/ws", handleWebSocket)

	startServer()

}

func newApplicationStore() *ApplicationStore {
	appStore := ApplicationStore{}
	appStore.initDB()
	appStore.initAPIHandler()
	appStore.registerAPIRoute()
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

func (this *ApplicationStore) initAPIHandler() {
	this.apiHandler = &APIHandler{db: this.dbStore}
}

func (this *ApplicationStore) registerAPIRoute() {
	http.HandleFunc("/messages", this.apiHandler.handleMessages)
	http.HandleFunc("/healthcheck", this.apiHandler.checkServerHealth)
}

func startServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("Server listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}