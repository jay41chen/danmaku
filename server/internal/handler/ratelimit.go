package handler

import (
	"math"
	"net/http"
	"strconv"
	"time"

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

// 回傳 true = 可以繼續；false = 已經回了 429，handler 要直接 return
func (this *AuthHandler) allow(w http.ResponseWriter, r *http.Request, rule rateRule, value string) bool {
	key := rule.key(value)
	allowed, retryAfter, err := this.limiter.Allow(r.Context(), key, rule.limit, rule.window)
	// 直接遇到limiter error
	if err != nil {
		// fail open：limiter（Redis）壞掉時放行，不讓所有人都登入不了
		this.logger.ErrorContext(r.Context(), "rate limiter failed, fail open",
			"rule", rule.name, "key", key, "error", err)
		return true
	}
	if !allowed {
		// 撞到rate limit
		prom.RateLimitedTotal.WithLabelValues(rule.name).Inc()
		this.logger.WarnContext(r.Context(), "rate limited",
			"rule", rule.name, "key", key, "remote_addr", r.RemoteAddr)

		// 無條件進位到秒，至少 1 秒：剩 0.4 秒不能回 0（client 會以為現在就能再試）
		seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)

		// header 要在 http.Error 之前設，它會把 header 送出去
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	return true
}
