package store

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// 固定時間窗口的限流：同一個 key 在 window 內最多 limit 次
// 實作：RedisStore（Lua script 把 INCR + PEXPIRE 包成一個原子操作）
type RateLimiterInterface interface {
	// allowed=false 時，retryAfter 是這個窗口還剩多久
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
	// 清掉 key 的計數（登入成功時用，避免使用者自己打錯幾次後被鎖）
	Reset(ctx context.Context, key string) error
	// 退還這一次 Allow 用掉的額度（-1）；key 已經過期就什麼都不做
	Refund(ctx context.Context, key string) error
}

// INCR 不會設過期時間：如果分成兩個指令送，INCR 成功、EXPIRE 沒送到（crash / 斷線），
// key 就會永久存在，那個 IP / 帳號被永久封鎖。包成 script，Redis 會整段跑完、中間不會被插隊。
// 不用 MULTI/EXEC：指令是排隊後一起執行，拿不到 INCR 的結果，沒辦法寫 if c == 1。
//
// 用 PEXPIRE / PTTL（毫秒）而不是 EXPIRE / TTL（秒）：window 可以不是整數秒
// 回傳 {count, 剩餘毫秒}：在同一段 script 裡讀 TTL，跟 count 是同一個時間點的值
var allowScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('PTTL', KEYS[1])}
`)

func (this *RedisStore) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	// Run 先送 EVALSHA（只送 script 的 hash），Redis 沒快取過才退回 EVAL 送整段
	result, err := allowScript.Run(ctx, this.client, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	count, ttlMillis := result[0], result[1]

	if count <= int64(limit) {
		return true, 0, nil
	}
	return false, time.Duration(ttlMillis) * time.Millisecond, nil
}

// key 在 Allow 和 Refund 之間剛好過期（例如 bcrypt 跑的那段時間）時，直接 DECR 會把不存在的 key 當 0 來減，
// 建出一個值是 -1、沒有 TTL 的 key：永久多一次額度，而且 key 永遠不會被清掉。
// 「key 還在才 DECR」拆成 EXISTS + DECR 兩個指令的話，中間還是可能過期（check-then-act），所以也包成 script
var refundScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
	redis.call('DECR', KEYS[1])
end
return 0
`)

func (this *RedisStore) Refund(ctx context.Context, key string) error {
	return refundScript.Run(ctx, this.client, []string{key}).Err()
}

func (this *RedisStore) Reset(ctx context.Context, key string) error {
	// key 不存在時 DEL 回 0，不算錯誤
	return this.client.Del(ctx, key).Err()
}
