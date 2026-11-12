package handler

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/ratelimit"
)

// 限流的機制（Rule / Check）在 internal/ratelimit；這裡只有 HTTP 層的部分：429 回應和 auth 用的 allow
// 具體的規則（上限多少）寫在用它的地方旁邊，例如 auth.go

// 限流器：interface 定義在使用方，理由同 auth.go 的 authStore
// 由 *redisstore.RedisStore 實作（Redis 計數，多台 server 共用）
// 固定時間窗口：同一個 key 在 window 內最多 limit 次
type rateLimiter interface {
	// allowed=false 時，retryAfter 是這個窗口還剩多久
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
	// 清掉 key 的計數（登入成功時用，避免使用者自己打錯幾次後被鎖）
	Reset(ctx context.Context, key string) error
	// 退還這一次 Allow 用掉的額度（-1）；key 已經過期就什麼都不做
	Refund(ctx context.Context, key string) error
}

// 呼叫 ratelimit.Check，並在被擋的 log 加上 remote_addr（auth 的 log 欄位維持不變）
func checkRate(r *http.Request, limiter rateLimiter, logger *slog.Logger, rule ratelimit.Rule, value string) (allowed bool, retryAfter time.Duration) {
	return ratelimit.Check(r.Context(), limiter, logger, rule, value, "remote_addr", r.RemoteAddr)
}

// 回 429 + Retry-After
func writeTooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	// 無條件進位到秒，至少 1 秒：剩 0.4 秒不能回 0（client 會以為現在就能再試）
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)

	// header 要在 WriteError 之前設，WriteError 一寫出 status 就把 header 送出去了
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
}

// 回傳 true = 可以繼續；false = 已經回了 429，handler 要直接 return
func (this *AuthHandler) allow(w http.ResponseWriter, r *http.Request, rule ratelimit.Rule, value string) bool {
	allowed, retryAfter := checkRate(r, this.limiter, this.logger, rule, value)
	if !allowed {
		writeTooManyRequests(w, retryAfter)
		return false
	}
	return true
}
