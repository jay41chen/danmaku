package redisstore

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/randx"
	"github.com/jay41chen/danmaku/server/internal/supervise"
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

// 我們的 client 預設值：沒帶 context 期限的呼叫，Redis 不回應（封包被丟）時最多等多久
const (
	defaultReadTimeout   = time.Second
	defaultDialTimeout   = time.Second
	defaultDialerRetries = 1
)

// 把 go-redis 過長的預設值換成我們的：URL 沒設（零值）才補，URL 有設的（例如 ?read_timeout=3s）照用
// 非零值一律不動：ReadTimeout 負值 = 不設逾時（go-redis init() 處理）；DialTimeout 負值 go-redis 不處理，會原樣交給 net.Dialer，等於 deadline 已過、撥號立刻失敗（URL 的 dial_timeout<=0 會被轉成 -1）。兩者我們都不動，保持 URL 的原意；DialerRetries 的 <=0 go-redis 會當成 5（options.go），
// 但 URL 設不了 dialer_retries，所以這個欄位實際上永遠是零值，目前走不到這條路徑
// go-redis 的預設 ReadTimeout 5s / DialTimeout 5s / DialerRetries 5 / MaxRetries 3，封包被丟時實測一個沒期限的呼叫要卡 20s ~ 100s 以上（ADR 0020）
// 改完之後，封包被丟（TCP 連不上）時，沒期限的呼叫最壞約 4 秒：每次嘗試最多 1 秒（既有連線的讀是 ReadTimeout 1s；新連線是 DialerRetries × DialTimeout = 1s），共 4 次（MaxRetries 3 不動）
// TCP 連得上但 Redis 不回應（process 卡住）時，新連線還要做 HELLO / AUTH 握手，這段讀也受 ReadTimeout 限制：每次嘗試最多約 2 秒，4 次約 8 秒
// WriteTimeout 沒設時跟著 ReadTimeout（go-redis init()），所以也是 1s
// 行為改變：我們沒設 PoolTimeout，它由 ReadTimeout 推導（ReadTimeout + 1s，options.go），所以從 6s 變成 2s：
// 連線池被占滿（Redis 正常、負載很高）時，沒期限的呼叫等不到連線會在 2 秒（原本 6 秒）後得到 ErrPoolTimeout，而 ErrPoolTimeout 會被重試（最多 4 次）
// 要更快的呼叫仍然自己傳 ctx 期限：ctx 期限和 socket 逾時取較早的（go-redis internal/pool/conn.go 的 deadline）
// PubSub 的 Receive 沒有 socket 逾時（timeout 0、ctx 是 TODO），不受 ReadTimeout 影響；
// 健康檢查的 ping 只寫不讀，寫入期限 = min(5s 的 ping ctx, WriteTimeout = 現在的 1s)；重連用新的 dial 設定（1s、只撥一次）
// DialerRetries=1 也降低了啟動時對「Redis 還沒好」的容忍：Redis 拒絕連線時，啟動的 Ping（cmd/server/main.go initRedis）
// 4 次嘗試在幾百 ms 內全部失敗、process 直接退出，原本約有 2 秒的緩衝；有 Redis healthcheck / depends_on 的環境不受影響
// 要不要讓啟動等 Redis 暫緩，留到部署時決定
func applyClientDefaults(opt *redis.Options) {
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = defaultReadTimeout
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = defaultDialTimeout
	}
	// dialer_retries 不能從 URL 設，所以實際上永遠是零值；仍只在零值時補，保持跟其他欄位一致
	if opt.DialerRetries == 0 {
		opt.DialerRetries = defaultDialerRetries
	}
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

	// ctx 的期限要能套到 socket 讀寫上，否則 Redis 不回應（封包被丟）時，一般指令會等 read timeout（go-redis 預設 5 秒；我們改成 1 秒，見 applyClientDefaults）再重試
	// 這個選項只影響一般指令 / pipeline，沒有期限的 ctx 行為不變
	// PubSub 的 SUB / UNSUB / Receive 本來就直接套用 ctx 期限，不受這個選項影響
	option.ContextTimeoutEnabled = true

	applyClientDefaults(option)
	client := redis.NewClient(option)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	logger.Info("connected")

	s := &RedisStore{
		client: client,
		// 8 bytes 隨機：只要不同 server 不撞名就好（撞名的話 A 會收到 B 的心跳，聾掉就偵測不到）
		heartbeat: heartbeatChannel(randx.URLText(8)),
		logger:    logger,
	}
	s.watchdog = newPubsubWatchdog(s.publishHeartbeat, s.rebuildPubSub, logger)
	s.pubsub.Store(client.Subscribe(ctx, s.heartbeat))
	s.rooms = newRoomSubscriptions(
		func(ctx context.Context, roomID string) error {
			return s.pubsub.Load().Subscribe(ctx, roomChannel(roomID))
		},
		func(ctx context.Context, roomID string) error {
			return s.pubsub.Load().Unsubscribe(ctx, roomChannel(roomID))
		},
		// room_subscriptions 的 debounce / retry / confirm timer：callback 跑在 runtime 建的 goroutine，
		// 直接 panic 會讓 process 掛掉但沒有 log / metric；Escalate 結果一樣（process 掛掉），只是先記錄下來（ADR 0023）
		func(d time.Duration, f func()) stopper {
			return time.AfterFunc(d, func() {
				supervise.Escalate(ctx, logger, "redis_room_timer", func(context.Context) { f() })
			})
		},
		logger,
	)
	// 送 SUB / UNSUB 的 worker：整台 server 一個，跟 process 活一樣久（main 傳進來的是 context.Background()）
	// 不能等到 Subscribe 才啟動：Subscribe 是 ws.go 另外起的 goroutine，順序不保證，
	// 第一個 Join 可能比它早到，標了 dirty 卻沒人處理
	// supervise.Escalate（第一次 panic 就讓 process 掛掉，ADR 0023）：不能原地重啟。desired / actual 雖然存在 rooms 裡，
	// 但 run 每一輪會把 dirty 整包取出到區域變數，reconcile 到一半 panic 的話，同一批還沒處理的房間就沒人會再標 dirty；
	// panic 那間的 state 已經改成 subSent、確認 timer 卻沒啟動，之後每次 Join 都會 timeout，要等所有人離開才恢復
	go supervise.Escalate(ctx, logger, "redis_rooms", s.rooms.run)

	// watchdog 同理：跟 process 一樣久，不能等 Subscribe 啟動（心跳是 Subscribe 的 consumer 收到才算數，但發送和判定不依賴它啟動順序）
	// ticker 在這裡建、傳進去：run 不自己建 ticker，test 才能用手動的 tick channel
	// Stop 放在 goroutine 裡：NewRedisStore return 後 ticker 還要繼續用，不能 defer 在這個函式；重啟時每次重建一個
	// supervise.Restart：判斷邏輯和發心跳的 panic 重啟就好（missCount 歸零，偵測晚一輪約 15 秒）
	// 接受的例外（ADR 0023）：rebuildPubSub 裡 go-redis 的 old.Close() 本身 panic 的話，舊的 PubSub 沒關掉，
	// consumer 會一直卡在聽不到的舊連線上，重啟 watchdog 修不好（之後的重建只會換掉上一輪新建的那個）。
	// 只有 go-redis 內部 panic 才會發生；發生時 worker_panics_total{worker="redis_watchdog"} 和 pubsub_deaf 都會增加，靠 alert 叫人重啟
	go supervise.Restart(ctx, logger, "redis_watchdog", func(ctx context.Context) {
		tick := time.NewTicker(heartbeatInterval)
		defer tick.Stop()
		s.watchdog.run(ctx, tick.C)
	})

	return s, nil
}

func (s *RedisStore) CheckHealth(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// 發一則心跳到自己的 heartbeat channel
// 開了 ContextTimeoutEnabled，500ms 的 ctx 會套到 socket 的讀寫上：Redis 卡住時這一輪最多約 500ms 就返回，
// 15 秒的間隔容得下，watchdog 不會卡死
// 這不算「廣播 Publish 失敗」，不 Inc 那個 metric
func (s *RedisStore) publishHeartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, redisCommandTimeout)
	defer cancel()
	return s.client.Publish(ctx, s.heartbeat, "ping").Err()
}

// watchdog 判定 PubSub 聾掉（連線還在、但收不到任何東西）時呼叫：換一個全新的 PubSub
//
// 順序：先 Store 新指標、再 Close 舊的。反過來的話，worker 在這中間 Load() 到已經 Close 的舊 PubSub，SUB 會失敗；
// 而 consumer 看到舊 channel 關閉時，要靠「指標已經不是 ps」判斷這是重建造成的關閉（見 Subscribe）
//
// 不在這裡重設房間（resetAfterRebuild）：一定要等 consumer 把舊 channel 收完才重設，理由見 Subscribe
func (s *RedisStore) rebuildPubSub(ctx context.Context) {
	prom.PubSubDeaf()
	s.logger.WarnContext(ctx, "pubsub judged deaf, rebuilding")
	fresh := s.client.Subscribe(ctx, s.heartbeat)
	old := s.pubsub.Swap(fresh)
	// Swap = Store 新的 + 回傳舊的，是同一個原子操作
	old.Close()
}
