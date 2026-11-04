package redisstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/redis/go-redis/v9"
)

// 跨 server 的房間在線人數：每間房一個 Sorted Set（room:<public_id>:members）
// member = "<conn_id>:<user_id>"（匿名 user_id = 0），score = 最後一次續期的 Redis 時間（秒）
// 時間一律用 Redis 自己的 TIME，不用各台 server 的時鐘，避免時鐘不同步
const (
	// 成員的 TTL（N）：go-notes 3.27 —— 每 X = 10 秒續期一次，N 要 > 2X 才撐得過掉一次續期；這裡取 30 秒
	// 同時用在兩個地方：key 整個的 EXPIRE（房間沒人續期就整個消失），以及計數時剔除 score 太舊的 member
	presenceTTL = 30 * time.Second
)

func presenceKey(room string) string {
	return "room:" + room + ":members"
}

func presenceMember(e model.PresenceEntry) string {
	return e.ConnID + ":" + strconv.FormatInt(e.UserID, 10)
}

// 解析 presenceMember 的結果；conn_id 是 base64url（沒有 ':'），所以從最後一個 ':' 切開
func parsePresenceMember(m string) (connID string, userID int64, ok bool) {
	i := strings.LastIndex(m, ":")
	if i < 0 {
		return "", 0, false
	}
	userID, err := strconv.ParseInt(m[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[:i], userID, true
}

// 參數編碼：KEYS = 每筆操作的 room key（先 add 再 remove，一筆一個，可重複）
// ARGV[1] = add 的筆數 nAdd；ARGV[1+i] 對應 KEYS[i] 的 member（i = 1..#KEYS）
// 前 nAdd 筆做 ZADD + EXPIRE，其餘做 ZREM
var syncPresenceScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1])
local nAdd = tonumber(ARGV[1])
for i = 1, #KEYS do
	local member = ARGV[i + 1]
	if i <= nAdd then
		redis.call('ZADD', KEYS[i], now, member)
		redis.call('EXPIRE', KEYS[i], tonumber(ARGV[#ARGV]))
	else
		redis.call('ZREM', KEYS[i], member)
	end
end
return 0
`)

// 一次 round trip 寫入：add = 加入 / 續期，remove = 移除
// 兩個都是空就不呼叫 Redis
// ARGV 最後一個放 TTL 秒數（避免 script 裡寫死 30）
func (this *RedisStore) SyncPresence(ctx context.Context, add, remove []model.PresenceEntry) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	keys := make([]string, 0, len(add)+len(remove))
	args := make([]any, 0, len(add)+len(remove)+2)
	args = append(args, len(add))
	for _, e := range add {
		keys = append(keys, presenceKey(e.Room))
		args = append(args, presenceMember(e))
	}
	for _, e := range remove {
		keys = append(keys, presenceKey(e.Room))
		args = append(args, presenceMember(e))
	}
	args = append(args, int(presenceTTL/time.Second))
	return syncPresenceScript.Run(ctx, this.client, keys, args...).Err()
}

// KEYS = 要算的房間 key；ARGV[1] = TTL 秒數
// 每個 key：先剔除 score < now - TTL 的 member（"(" = 不含邊界），再回傳剩下的全部 member
var countPresenceScript = redis.NewScript(`
local t = redis.call('TIME')
local cutoff = tonumber(t[1]) - tonumber(ARGV[1])
local result = {}
for i = 1, #KEYS do
	redis.call('ZREMRANGEBYSCORE', KEYS[i], '-inf', '(' .. cutoff)
	result[i] = redis.call('ZRANGE', KEYS[i], 0, -1)
end
return result
`)

// 一次 round trip 算出每間房的人數；結果一定包含每個傳進來的房間（沒人 = 0）
// 去重：登入的人（user_id != 0）同一個 user 只算 1（多分頁、多台 server）；匿名每條連線各算 1
func (this *RedisStore) CountPresence(ctx context.Context, rooms []string) (map[string]int, error) {
	counts := make(map[string]int, len(rooms))
	if len(rooms) == 0 {
		return counts, nil
	}
	keys := make([]string, len(rooms))
	for i, r := range rooms {
		keys[i] = presenceKey(r)
	}
	raw, err := countPresenceScript.Run(ctx, this.client, keys, int(presenceTTL/time.Second)).Slice()
	if err != nil {
		return nil, err
	}
	if len(raw) != len(rooms) {
		return nil, fmt.Errorf("count presence: got %d lists, want %d", len(raw), len(rooms))
	}
	for i, room := range rooms {
		list, ok := raw[i].([]any)
		if !ok {
			return nil, fmt.Errorf("count presence: unexpected reply type %T", raw[i])
		}
		seen := make(map[string]struct{}, len(list))
		for _, item := range list {
			m, _ := item.(string)
			connID, userID, ok := parsePresenceMember(m)
			if !ok {
				continue
			}
			if userID != 0 {
				seen["u:"+strconv.FormatInt(userID, 10)] = struct{}{}
			} else {
				seen["c:"+connID] = struct{}{}
			}
		}
		counts[room] = len(seen)
	}
	return counts, nil
}
