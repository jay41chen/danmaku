package redisstore

import (
	"context"
	"fmt"
	"slices"
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
	// 成員的 TTL（N）：每 X = 10 秒續期一次，N 要 > 2X 才撐得過掉一次續期；這裡取 30 秒
	// 同時用在兩個地方：key 整個的 EXPIRE（房間沒人續期就整個消失），以及計數時剔除 score 太舊的 member
	presenceTTL = 30 * time.Second
)

func presenceKey(room string) string {
	return "room:" + room + ":members"
}

// 熱門房間排行：Sorted Set，member = 房間 public_id，score = 人數
// 房間還在不在排行上，以 room:<id>:members 這個 key 是否存在為準，不是看這裡的 score（見 TopRooms）
const viewerRankingKey = "room:viewer_ranking"

// TopRooms 每批多撈幾筆：stale 的房間會被濾掉，多撈一點可以少跑幾輪
const topRoomsExtraPerBatch = 10

// TopRooms 最多跑幾輪：防止排行上塞滿 stale 房間時一直迴圈
const topRoomsMaxRounds = 10

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

// 把這一輪算出的人數寫進排行：count > 0 → ZADD，count == 0 → ZREM
// 一個 pipeline（一次 round trip）送出一個 ZADD（多個 member）+ 一個 ZREM；空 map 不呼叫 Redis
// 用途：presenceWorker 每次 CountPresence 之後
func (this *RedisStore) UpdateViewerRanking(ctx context.Context, counts map[string]int) error {
	if len(counts) == 0 {
		return nil
	}
	var (
		adds []redis.Z
		rems []any
	)
	for room, n := range counts {
		if n > 0 {
			adds = append(adds, redis.Z{Score: float64(n), Member: room})
		} else {
			rems = append(rems, room)
		}
	}
	_, err := this.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		if len(adds) > 0 {
			p.ZAdd(ctx, viewerRankingKey, adds...)
		}
		if len(rems) > 0 {
			p.ZRem(ctx, viewerRankingKey, rems...)
		}
		return nil
	})
	return err
}

// 人數前 n 名的房間（由多到少）；n <= 0 回空
// 排行裡可能有 stale 的房間（最後一個人離開、server 當掉、key 過期），所以每批都用 EXISTS room:<id>:members 過濾，
// 不存在的順手 ZREM（lazy cleanup）；撈到 n 間活的或排行用完就停
//
// 不需要原子性：每個指令都只碰一個 key（Redis Cluster 友善，不用 Lua 跨 key）；
// EXISTS 和 ZREM 之間房間剛好有人進來的 race 可以接受：最多少列一次，5 秒內 worker 會把它寫回排行
// 輪數有上限（topRoomsMaxRounds），結果可能少於 n
//
// 跨批之間分數可能被更新（其他 server 每 5 秒 ZADD），同一間房可能被撈到兩次，所以要去重；
// 跳過的房間 5 秒內會自我修正（下次呼叫就會正常出現）。
// 去重用線性搜尋而不是 map：n 最多 model.MaxRoomListLimit（50），O(n²) 也只有幾千次比較，比 map 好讀；
// 若放寬上限就改用 map
func (this *RedisStore) TopRooms(ctx context.Context, n int) ([]model.RoomViewers, error) {
	result := []model.RoomViewers{}
	if n <= 0 {
		return result, nil
	}
	batchSize := int64(n + topRoomsExtraPerBatch)
	// start 只隨「留下來的」前進：被 ZREM 的會讓後面的往前補位，不用跳過
	var start int64
	for round := 0; round < topRoomsMaxRounds && len(result) < n; round++ {
		batch, err := this.client.ZRevRangeWithScores(ctx, viewerRankingKey, start, start+batchSize-1).Result()
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		cmds, err := this.client.Pipelined(ctx, func(p redis.Pipeliner) error {
			for _, z := range batch {
				p.Exists(ctx, presenceKey(z.Member.(string)))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		var stale []any
		for i, z := range batch {
			room := z.Member.(string)
			if cmds[i].(*redis.IntCmd).Val() == 0 {
				stale = append(stale, room)
				continue
			}
			// 重複的房間仍留在排行上、佔著一個位置，所以 start 照樣前進（否則下一批會從同一個位置重讀）
			// 只有 len(result) >= n 時不動 start：這時迴圈會直接結束，不會再用到
			if len(result) < n {
				start++
				if slices.ContainsFunc(result, func(r model.RoomViewers) bool { return r.PublicID == room }) {
					continue
				}
				result = append(result, model.RoomViewers{PublicID: room, Count: int(z.Score)})
			}
		}
		if len(stale) > 0 {
			if err := this.client.ZRem(ctx, viewerRankingKey, stale...).Err(); err != nil {
				return nil, err
			}
		}
		if int64(len(batch)) < batchSize {
			break // 排行用完了
		}
	}
	return result, nil
}
