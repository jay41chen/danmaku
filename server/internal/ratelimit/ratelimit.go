// Package ratelimit 是限流的「機制」：規則長什麼樣、怎麼組 key、問 limiter 並處理 fail open / fail closed / log / metric
// 具體的規則（上限多少）寫在用它的地方旁邊，例如 usecase/auth.go、usecase/message.go
package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 限流的時間預算：Redis 不回應時要在這段時間內放棄（依規則 fail open 或 fail closed），不能讓 request 卡住
// 本機 Redis 一次呼叫遠低於 1ms、M6 POST p99 約 6ms，200ms 很寬鬆
const checkTimeout = 200 * time.Millisecond

// 一條限流規則：同一個 value 在 window 內最多 limit 次
// Name 也是 Redis key 和 log 的一部分
type Rule struct {
	Name   string
	Limit  int
	Window time.Duration
	// limiter 出錯或超時、查不到結果時擋下（回 model.ErrRateLimiterUnavailable）；零值 false = fail open（放行）
	FailClosed bool
}

// 限流用的 Redis key：rl:<規則>:<值>，例如 rl:login:username:jay
// value 決定「按什麼分開數」：傳 IP 就是每個 IP 一個計數器，傳 username 就是每個帳號一個
func (rule Rule) Key(value string) string {
	return "rl:" + rule.Name + ":" + value
}

// 限流器：interface 定義在使用方（ADR 0009），由 *redisstore.RedisStore 實作（Redis 計數，多台 server 共用）
// 固定時間窗口：同一個 key 在 window 內最多 limit 次
// Check 只需要 Allow，所以參數用這個最小 interface，呼叫端較寬的 interface（含 Reset / Refund）也能直接傳進來
type Limiter interface {
	// allowed=false 時，retryAfter 是這個窗口還剩多久
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// 問 limiter 這條規則放不放行；被擋時回傳還要等多久（Retry-After 用）
// 這裡負責 fail open / fail closed、記 log、累計 metric；回不回 429 由呼叫端決定
// limiter 出錯或超時：rule.FailClosed 時回 (false, 0, model.ErrRateLimiterUnavailable)，否則 fail open 回 (true, 0, nil)
// 沒出錯時 err 一定是 nil
// （login 一條規則擋住就直接回；POST /rooms/{public_id}/messages 兩條都要問，取比較長的等待時間）
// logAttrs 附加在「被擋」的 warn log 上（例如 HTTP 層傳 "remote_addr", r.RemoteAddr）
func Check(ctx context.Context, limiter Limiter, logger *slog.Logger, rule Rule, value string, logAttrs ...any) (allowed bool, retryAfter time.Duration, err error) {
	key := rule.Key(value)
	allowCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	allowed, retryAfter, err = limiter.Allow(allowCtx, key, rule.Limit, rule.Window)
	// 直接遇到limiter error
	if err != nil {
		prom.RateLimiterError(rule.Name)
		if rule.FailClosed {
			// fail closed：limiter（Redis）壞掉時擋下，不讓攻擊者趁限流失效無限猜密碼
			logger.ErrorContext(ctx, "rate limiter failed, fail closed",
				"rule", rule.Name, "key", key, "error", err)
			return false, 0, model.ErrRateLimiterUnavailable
		}
		// fail open：limiter（Redis）壞掉時放行，不讓所有人都發不了訊息
		logger.ErrorContext(ctx, "rate limiter failed, fail open",
			"rule", rule.Name, "key", key, "error", err)
		return true, 0, nil
	}
	if !allowed {
		// 撞到rate limit
		prom.RateLimited(rule.Name)
		attrs := append([]any{"rule", rule.Name, "key", key}, logAttrs...)
		logger.WarnContext(ctx, "rate limited", attrs...)
	}
	return allowed, retryAfter, nil
}
