package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/store"
)

type HealthHandler struct {
	db     store.DBStoreInterface
	redis  store.RedisStoreInterface
	logger *slog.Logger
}

// constructor
func NewHealthHandler(db store.DBStoreInterface, redis store.RedisStoreInterface) *HealthHandler {
	return &HealthHandler{
		db:     db,
		redis:  redis,
		logger: slog.Default().With("component", "health"),
	}
}

// check server is alive
func (this *HealthHandler) HandleHealthCheck(w http.ResponseWriter, r *http.Request) {
	// method 由 router 限定（main.go 的 publicGet）；Go 1.22 的 "GET /x" 也會比對 HEAD，
	// 所以跟原本手寫的 GET + HEAD 行為一樣
	/// no need check cors since its just a health check api
	httpx.SetContentType(w)

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
			errorMsg["db"] = "error"
		}
		if redisError != nil {
			errorMsg["redis"] = "error"
		}
		w.WriteHeader(http.StatusInternalServerError)
		this.logger.ErrorContext(r.Context(), "health check failed", "dbError", dbError, "redisError", redisError)
		json.NewEncoder(w).Encode(errorMsg)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
