package handler

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 限流的「機制」：規則長什麼樣、怎麼組 key、被擋時怎麼回應
// 具體的規則（上限多少）寫在用它的 handler 旁邊，例如 auth.go

// 一條限流規則：同一個 value 在 window 內最多 limit 次
// name 也是 Redis key 和 log 的一部分
type rateRule struct {
	name   string
	limit  int
	window time.Duration
}

// 限流用的 Redis key：rl:<規則>:<值>，例如 rl:login:username:jay
// value 決定「按什麼分開數」：傳 IP 就是每個 IP 一個計數器，傳 username 就是每個帳號一個
func (rule rateRule) key(value string) string {
	return "rl:" + rule.name + ":" + value
}

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

// 問 limiter 這條規則放不放行；被擋時回傳還要等多久（Retry-After 用）
// 這裡負責 fail open、記 log、累計 metric；回不回 429 由呼叫端決定
// （login 一條規則擋住就直接回；POST /rooms/{public_id}/messages 兩條都要問，取比較長的等待時間）
func checkRate(r *http.Request, limiter rateLimiter, logger *slog.Logger, rule rateRule, value string) (allowed bool, retryAfter time.Duration) {
	key := rule.key(value)
	allowed, retryAfter, err := limiter.Allow(r.Context(), key, rule.limit, rule.window)
	// 直接遇到limiter error
	if err != nil {
		// fail open：limiter（Redis）壞掉時放行，不讓所有人都登入不了
		logger.ErrorContext(r.Context(), "rate limiter failed, fail open",
			"rule", rule.name, "key", key, "error", err)
		return true, 0
	}
	if !allowed {
		// 撞到rate limit
		prom.RateLimitedTotal.WithLabelValues(rule.name).Inc()
		logger.WarnContext(r.Context(), "rate limited",
			"rule", rule.name, "key", key, "remote_addr", r.RemoteAddr)
	}
	return allowed, retryAfter
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
func (this *AuthHandler) allow(w http.ResponseWriter, r *http.Request, rule rateRule, value string) bool {
	allowed, retryAfter := checkRate(r, this.limiter, this.logger, rule, value)
	if !allowed {
		writeTooManyRequests(w, retryAfter)
		return false
	}
	return true
}
