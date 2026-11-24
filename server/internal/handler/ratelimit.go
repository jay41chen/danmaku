package handler

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 限流的機制（Rule / Check）在 internal/ratelimit，規則由各 use case 擁有；這裡只有 HTTP 層的部分：429 / 503 回應

// 回 429 + Retry-After
func writeTooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	// 無條件進位到秒，至少 1 秒：剩 0.4 秒不能回 0（client 會以為現在就能再試）
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)

	// header 要在 WriteError 之前設，WriteError 一寫出 status 就把 header 送出去了
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
}

// err 是 *model.RateLimitedError 就回 429 + Retry-After 並回傳 true（handler 直接 return）
// err 是 model.ErrRateLimiterUnavailable 就回 503 auth_unavailable（不帶 Retry-After：不知道 Redis 何時恢復）並回傳 true
// 目前只有 auth 的規則是 fail closed，所以錯誤碼叫 auth_unavailable；之後有別的規則 fail closed 再改
// 都不是就什麼都不寫、回傳 false
// 用途：各 handler 把 use case 回的限流錯誤翻成 HTTP
func writeIfRateLimited(w http.ResponseWriter, err error) bool {
	var limited *model.RateLimitedError
	if errors.As(err, &limited) {
		writeTooManyRequests(w, limited.RetryAfter)
		return true
	}
	if errors.Is(err, model.ErrRateLimiterUnavailable) {
		httpx.WriteError(w, http.StatusServiceUnavailable, "auth_unavailable", "service temporarily unavailable")
		return true
	}
	return false
}
