package redisstore

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/randx"
	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
	// 整台 server 同一時間只有一個 PubSub，房間的 channel 動態 SUBSCRIBE / UNSUBSCRIBE（只固定訂一條自己的心跳 channel）
	// 用 atomic.Pointer：watchdog 判定聾掉時會換一個新的（rebuildPubSub），SUB / UNSUB 的 worker 和 Subscribe 的 consumer 在別的 goroutine，
	// 每次都要 Load() 拿當下的，不能捕捉舊的指標（舊的已經 Close，寫進去只會失敗）
	pubsub atomic.Pointer[redis.PubSub]
	rooms  *roomSubscriptions
	// 這台 server 自己的心跳 channel（heartbeatChannel 命名），啟動時產生一次，重建 PubSub 也沿用
	heartbeat string
	watchdog  *pubsubWatchdog
	logger    *slog.Logger
}

// constuctor
func NewRedisStore(ctx context.Context, address string) (*RedisStore, error) {
	logger := slog.Default().With("component", "redis")

	option, err := redis.ParseURL(address)
	if err != nil {
		// 不 fallback 成 Options{Addr: address}：URL 寫錯時整串（含密碼）會變成連線位址，被印進 log
		// ParseURL 自己的錯誤訊息也會帶出 URL，所以回固定訊息
		return nil, errors.New("invalid redis url: want redis:// or rediss://[[user]:password@]host[:port][/db-number], special characters in password must be percent-encoded")
	}

	client := redis.NewClient(option)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	logger.Info("connected")

	this := &RedisStore{
		client: client,
		// 8 bytes 隨機：只要不同 server 不撞名就好（撞名的話 A 會收到 B 的心跳，聾掉就偵測不到）
		heartbeat: heartbeatChannel(randx.URLText(8)),
		logger:    logger,
	}
	this.watchdog = newPubsubWatchdog(this.publishHeartbeat, this.rebuildPubSub, logger)
	this.pubsub.Store(client.Subscribe(ctx, this.heartbeat))
	this.rooms = newRoomSubscriptions(
		func(ctx context.Context, roomID string) error {
			return this.pubsub.Load().Subscribe(ctx, roomChannel(roomID))
		},
		func(ctx context.Context, roomID string) error {
			return this.pubsub.Load().Unsubscribe(ctx, roomChannel(roomID))
		},
		func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		logger,
	)
	// 送 SUB / UNSUB 的 worker：整台 server 一個，跟 process 活一樣久（main 傳進來的是 context.Background()）
	// 不能等到 Subscribe 才啟動：Subscribe 是 ws.go 另外起的 goroutine，順序不保證，
	// 第一個 Join 可能比它早到，標了 dirty 卻沒人處理
	go this.rooms.run(ctx)

	// watchdog 同理：跟 process 一樣久，不能等 Subscribe 啟動（心跳是 Subscribe 的 consumer 收到才算數，但發送和判定不依賴它啟動順序）
	// ticker 在這裡建、傳進去：run 不自己建 ticker，test 才能用手動的 tick channel
	// Stop 放在 goroutine 裡：NewRedisStore return 後 ticker 還要繼續用，不能 defer 在這個函式
	go func() {
		tick := time.NewTicker(heartbeatInterval)
		defer tick.Stop()
		this.watchdog.run(ctx, tick.C)
	}()

	return this, nil
}

func (this *RedisStore) CheckHealth(ctx context.Context) error {
	return this.client.Ping(ctx).Err()
}

// 發一則心跳到自己的 heartbeat channel
// 500ms 的 ctx 限制不了 socket I/O：沒開 ContextTimeoutEnabled，go-redis 不會把 ctx 的期限套到 socket 的讀寫上
// （實測 PUBLISH 不回應時約 5 秒才返回）。所以 Redis 卡住時 watchdog 只是這一輪多等幾秒，15 秒的間隔容得下，不會卡死
// 這不算「廣播 Publish 失敗」，不 Inc 那個 metric
func (this *RedisStore) publishHeartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, redisCommandTimeout)
	defer cancel()
	return this.client.Publish(ctx, this.heartbeat, "ping").Err()
}

// watchdog 判定 PubSub 聾掉（連線還在、但收不到任何東西）時呼叫：換一個全新的 PubSub
//
// 順序：先 Store 新指標、再 Close 舊的。反過來的話，worker 在這中間 Load() 到已經 Close 的舊 PubSub，SUB 會失敗；
// 而 consumer 看到舊 channel 關閉時，要靠「指標已經不是 ps」判斷這是重建造成的關閉（見 Subscribe）
//
// 不在這裡重設房間（resetAfterRebuild）：一定要等 consumer 把舊 channel 收完才重設，理由見 Subscribe
func (this *RedisStore) rebuildPubSub(ctx context.Context) {
	prom.PubSubDeafTotal.Inc()
	this.logger.WarnContext(ctx, "pubsub judged deaf, rebuilding")
	fresh := this.client.Subscribe(ctx, this.heartbeat)
	old := this.pubsub.Swap(fresh)
	// Swap = Store 新的 + 回傳舊的，是同一個原子操作
	old.Close()
}
