package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/store"
)

type APIHandler struct {
	db     store.DBStoreInterface
	redis  store.RedisStoreInterface
	logger *slog.Logger
}

// constructor
func NewAPIHandler(db store.DBStoreInterface, redis store.RedisStoreInterface) *APIHandler {
	return &APIHandler{
		db:     db,
		redis:  redis,
		logger: slog.Default().With("component", "api"),
	}
}

func setContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func checkMethodValidation(r *http.Request, allowMethods []string) bool {
	return slices.Contains(allowMethods, r.Method)
}

func (this *APIHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodGet}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	setContentType(w)

	msgs, err := this.db.QueryMessage(r.Context(), 50)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "query messages failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(msgs)
}

// check server is alive
func (this *APIHandler) CheckServerHealth(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodGet, http.MethodHead}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
