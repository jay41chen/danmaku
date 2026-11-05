// Prometheus metric 定義。強綁定 Prometheus client library。
// 目前規模不需要抽 interface；未來若需替換，改這個 package 即可。
package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HttpRequestCount = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status_code"},
	)
	HttpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Duration of HTTP requests in seconds",
		},
		[]string{"method", "path"},
	)
	// label 只放規則名稱（login:ip 等，固定幾個），不放 IP / username：每個值都會變成一條新的時間序列
	RateLimitedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limited_total",
			Help: "Total number of requests rejected by rate limiting",
		},
		[]string{"rule"},
	)
	WebsocketConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "websocket_connections_active",
			Help: "Number of active WebSocket connections",
		},
	)
	// label 只有 member / anonymous 兩個值，不放 user_id：每個 user 都會變成一條新的時間序列
	// 數字只代表「這台 server」：member = 這台上不重複的登入 user 數，anonymous = 匿名連線數
	// 跨 server 的不重複 member 數是 W6 的事
	WebsocketViewers = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "websocket_viewers",
			Help: "Number of WebSocket viewers on this server by auth state (member = distinct users, anonymous = connections)",
		},
		[]string{"auth"},
	)
	// 以下幾個 metric 都沒有 room label：room 是 client 傳來的字串，每個值都會變成一條新的時間序列（見 ADR 0007）
	// 發到 Redis 失敗的次數（POST 訊息之後的廣播）；心跳的 publish 不算，那是 watchdog 自己的事
	PublishFailedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_publish_failed_total",
			Help: "Total number of failed Redis PUBLISH calls for message broadcast (heartbeat excluded)",
		},
	)
	// watchdog 判定 PubSub 聾掉（連續收不到自己發的心跳）而重建的次數
	PubSubDeafTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_pubsub_deaf_total",
			Help: "Total number of times the PubSub connection was judged deaf and rebuilt",
		},
	)
	// 重建 PubSub 之後，每個還有人在看的房間從重設到重新訂閱確認花的時間
	PubSubResubscribeDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "redis_pubsub_resubscribe_duration_seconds",
			Help:    "Time from PubSub rebuild until a room's subscription is confirmed again",
			Buckets: prometheus.DefBuckets,
		},
	)
	// 累計接受的 WS 連線數（Hub.Register 成功才算，連線數滿被拒的不算）
	WebsocketConnectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "websocket_connections_total",
			Help: "Total number of accepted WebSocket connections",
		},
	)
	// 送出 resync frame 的次數（PubSub 重建時 viewers > 0 的房間，每個房間每次重建最多一次，不是每條連線）
	WebsocketResyncTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "websocket_resync_sent_total",
			Help: "Total number of resync frames broadcast after a PubSub rebuild (rooms with viewers > 0 at rebuild time, at most once per room per rebuild)",
		},
	)
)
