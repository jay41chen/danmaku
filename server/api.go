package main

import (
	"encoding/json"
	"net/http"
	"os"
	"context"
	"sync"
	"time"
)

func handleMessages(w http.ResponseWriter, r *http.Request) {
	origin := os.Getenv("CORS_ORIGIN")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Content-Type", "application/json")

	msgs, err := GetMessages(50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(msgs)
}

// check server is alive
func checkServerHealth(w http.ResponseWriter, r *http.Request) {
	/// no need check cors since its just a health check api
	w.Header().Set("Content-Type", "application/json")

	/// setting timeout
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// goroutine, fire 2 parallel method
	var wg sync.WaitGroup
	var dbError, redisError error
	wg.Add(2)
	go func() { 
		defer wg.Done()
		dbError = CheckDBHealth(ctx)
	}()
	go func() {
		defer wg.Done()
		redisError = CheckRedisHealth(ctx)
	}()
	wg.Wait()

	if dbError != nil || redisError != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(
			map[string]string{
				"status":"internal error",
				"redis error": redisError.Error(),
				"db error": dbError.Error(),
			})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status":"ok"})
}


