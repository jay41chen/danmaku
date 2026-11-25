package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
)

// 這個 handler 需要的 DB / Redis 方法（為什麼定義在這裡見 ADR 0009）
// DB 和 Redis 的 CheckHealth 簽名一樣，所以兩邊共用這一個 interface
type healthChecker interface {
	CheckHealth(ctx context.Context) error
}

type HealthHandler struct {
	db     healthChecker
	redis  healthChecker
	logger *slog.Logger
}

// constructor
func NewHealthHandler(db healthChecker, redis healthChecker) *HealthHandler {
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
		dbError = this.checkSafely(ctx, "db", func() error { return this.db.CheckHealth(ctx) })
	}()
	go func() {
		defer wg.Done()
		redisError = this.checkSafely(ctx, "redis", func() error { return this.redis.CheckHealth(ctx) })
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
		this.logger.ErrorContext(r.Context(), "health check failed", "dbError", dbError, "redisError", redisError)
		httpx.WriteJSON(w, r, this.logger, http.StatusInternalServerError, errorMsg)
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, map[string]string{"status": "ok"})
}

// 呼叫 check，panic 時轉成 error 回傳（health check 不能因為某個依賴的 client panic 就讓 process 掛掉）
// 為什麼需要：check 跑在 HandleHealthCheck 自己開的 goroutine 上，RecoverPanic 接不到（recover 只對同一個 goroutine 有效）
// 同步函式、不開 goroutine：go 和 wg.Done() 留在呼叫端，賦值完成才會 Done，不用排 defer 的順序
// err 用具名回傳值：panic 後 return check() 跑不到，只能靠 defer 改 err
// 同時把 stacktrace 記進 log；回傳的 error 保持簡短，因為呼叫端會再 log 一次
// （TestHealthCheckPanicBecomesError）
func (this *HealthHandler) checkSafely(ctx context.Context, name string, check func() error) (err error) {
	defer func() {
		rec := recover()
		switch {
		case rec != nil:
			this.logger.ErrorContext(ctx, "health check panic recovered", "check", name, "panic", rec, "stacktrace", string(debug.Stack()))
			err = fmt.Errorf("%s health check panicked: %v", name, rec)
		}
	}()
	return check()
}
