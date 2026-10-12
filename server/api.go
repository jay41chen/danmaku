package main

import (
	"encoding/json"
	"net/http"
	"os"
	"context"
	"sync"
	"time"
)

type APIHandler struct {
	db DBStoreInterface
	redis RedisStoreInterface
}

// constructor
func NewAPIHandler(db DBStoreInterface, redis RedisStoreInterface,) *APIHandler {
	return &APIHandler{
		db: db,
		redis: redis,
	}
}

func setCors(w http.ResponseWriter){
	origin := os.Getenv("CORS_ORIGIN")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)	
}

func setContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func (this *APIHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	setContentType(w)
	
	msgs, err := this.db.QueryMessage(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return 
	}

	json.NewEncoder(w).Encode(msgs)
}

// check server is alive
func (this *APIHandler) checkServerHealth(w http.ResponseWriter, r *http.Request) {
	/// no need check cors since its just a health check api
	setContentType(w)

	/// setting timeout
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// goroutine, fire 2 parallel method
	var wg sync.WaitGroup
	var dbError, redisError error
	// 等redis弄完再加上
	wg.Add(2)
	go func() { 
		defer wg.Done()
		dbError = this.db.CheckHealth(ctx)
	}()
	go func() {
		defer wg.Done()
		redisError = this.redis.CheckHealth(ctx)
	}()
	wg.Wait()

	if dbError != nil || redisError != nil {
		errorMsg := map[string]string{}
		if dbError != nil {
			errorMsg["db error"] = dbError.Error()
		}
		if redisError != nil {
			errorMsg["redis error"]= redisError.Error()
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorMsg)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status":"ok"})
}
