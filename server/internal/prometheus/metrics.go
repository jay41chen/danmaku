// Prometheus metric 的唯一入口：其他 package 只呼叫這裡用事件命名的函式（PublishFailed、WSConnected…），
// 不直接碰 Prometheus 的型別或 API，metric 變數也不匯出
// 之後要換成別的 telemetry，只改這個 package 和 promtest，呼叫端和測試的斷言不用動
// metric 是 process 全域狀態（promauto 註冊在 default registry）：目前規模接受，不做 DI
package prometheus

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestCount = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status_code"},
	)
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Duration of HTTP requests in seconds",
		},
		[]string{"method", "path"},
	)
	// label 只放規則名稱（login:ip 等，固定幾個），不放 IP / username：每個值都會變成一條新的時間序列
	rateLimitedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limited_total",
			Help: "Total number of requests rejected by rate limiting",
		},
		[]string{"rule"},
	)
	// 全站共用的背景 worker panic 的次數（supervise.Restart / Escalate，不論之後原地重啟還是讓 process 掛掉）；alert 條件：有增加
	// label 是程式碼裡寫死的 worker 名稱（broadcast、presence…），值的數量固定
	workerPanicsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "worker_panics_total",
			Help: "Total number of recovered panics in server-wide background workers, by worker",
		},
		[]string{"worker"},
	)
	// label 只放規則名稱，理由同 rateLimitedTotal
	rateLimiterErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limiter_errors_total",
			Help: "Total number of rate limiter errors (Redis failure or timeout), whether the request was then allowed or rejected",
		},
		[]string{"rule"},
	)
	// label 只有 missing / unparsable 兩個值（httpx.ClientIPFallback*），不放 header 的值：值是 client 控制的字串
	clientIPFallbackTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "client_ip_fallback_total",
			Help: "Total number of requests whose configured client IP header was unusable, falling back to RemoteAddr",
		},
		[]string{"reason"},
	)
	websocketConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "websocket_connections_active",
			Help: "Number of active WebSocket connections",
		},
	)
	// label 只有 member / anonymous 兩個值，不放 user_id：每個 user 都會變成一條新的時間序列
	// 數字只代表「這台 server」：member = 這台上不重複的登入 user 數，anonymous = 匿名連線數
	// 跨 server 的不重複 member 數是 W6 的事
	websocketViewers = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "websocket_viewers",
			Help: "Number of WebSocket viewers on this server by auth state (member = distinct users, anonymous = connections)",
		},
		[]string{"auth"},
	)
	// 以下幾個 metric 都沒有 room label：room 是 client 傳來的字串，每個值都會變成一條新的時間序列（見 ADR 0007）
	// 發到 Redis 失敗的次數（POST 訊息之後的廣播）；心跳的 publish 不算，那是 watchdog 自己的事
	publishFailedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_publish_failed_total",
			Help: "Total number of failed Redis PUBLISH calls for message broadcast (heartbeat excluded)",
		},
	)
	// 某間房先前有訊息 PUBLISH 失敗，之後補發 resync frame 成功的次數（延遲補發，見 ResyncPublisher）
	deferredResyncPublishedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_deferred_resync_published_total",
			Help: "Total number of resync frames published to a room channel after an earlier failed message publish to that room (deferred resync)",
		},
	)
	// watchdog 判定 PubSub 聾掉（連續收不到自己發的心跳）而重建的次數
	pubSubDeafTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_pubsub_deaf_total",
			Help: "Total number of times the PubSub connection was judged deaf and rebuilt",
		},
	)
	// 已經確認的房間又收到訂閱確認：go-redis 重連後自動重新 SUBSCRIBE，或重送 SUBSCRIBE（確認 timer 到期，或 Redis 斷線時 SUBSCRIBE 寫入失敗後重送）；每次重新確認、每間房各 +1
	pubSubReconfirmTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "redis_pubsub_reconfirm_total",
			Help: "Subscribe confirmations for a room that was already confirmed: a go-redis reconnect, or a resent SUBSCRIBE (confirm timeout, or a write failure while Redis was down). Counted per room.",
		},
	)
	// 重建 PubSub 之後，每個還有人在看的房間從重設到重新訂閱確認花的時間
	pubSubResubscribeDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "redis_pubsub_resubscribe_duration_seconds",
			Help:    "Time from PubSub rebuild until a room's subscription is confirmed again",
			Buckets: prometheus.DefBuckets,
		},
	)
	// 累計接受的 WS 連線數（Hub.Register 成功才算，連線數滿被拒的不算）
	websocketConnectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "websocket_connections_total",
			Help: "Total number of accepted WebSocket connections",
		},
	)
	// 送出 resync frame 的次數，兩種來源：
	//   - PubSub 重建後（重建當下 viewers > 0 的房間，每個房間每次重建最多一次；確認前房間就被刪掉的沒有）
	//   - reconfirm（go-redis 重連或重複的訂閱確認，viewers > 0 的房間，每次確認一次）
	// 以房間為單位，不是每條連線
	websocketResyncTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "websocket_resync_sent_total",
			Help: "Total number of resync frames broadcast, either after a PubSub rebuild (at most once per room with viewers per rebuild) or on a reconfirm (a go-redis reconnect or a duplicate subscribe confirmation, for rooms with viewers)",
		},
	)
)

// HTTP 請求結束：計數（method、path、status）並記錄延遲
func HTTPRequest(method, path string, status int, d time.Duration) {
	httpRequestCount.WithLabelValues(method, path, strconv.Itoa(status)).Inc()
	httpRequestDuration.WithLabelValues(method, path).Observe(d.Seconds())
}

// 請求被 rate limit 規則擋下
func RateLimited(rule string) {
	rateLimitedTotal.WithLabelValues(rule).Inc()
}

// 背景 worker panic 被接住（之後會原地重啟，或超過上限讓 process 掛掉）
func WorkerPanic(worker string) {
	workerPanicsTotal.WithLabelValues(worker).Inc()
}

// 限流器（Redis）出錯或超時；不管之後是放行（fail open）還是擋下（fail closed）都會計
func RateLimiterError(rule string) {
	rateLimiterErrorsTotal.WithLabelValues(rule).Inc()
}

// 有設定 client IP header 但這次請求用不了，退回 RemoteAddr；reason 是 httpx.ClientIPFallback*
func ClientIPFallback(reason string) {
	clientIPFallbackTotal.WithLabelValues(reason).Inc()
}

// 訊息廣播 PUBLISH 到 Redis 失敗
func PublishFailed() {
	publishFailedTotal.Inc()
}

// 延遲補發的 resync frame 發送成功
func DeferredResyncPublished() {
	deferredResyncPublishedTotal.Inc()
}

// WS 連線被接受：在線數 +1、累計連線數 +1
func WSConnected() {
	websocketConnections.Inc()
	websocketConnectionsTotal.Inc()
}

// WS 連線離開：在線數 -1
func WSDisconnected() {
	websocketConnections.Dec()
}

// 這台 server 的 viewer 數：members 是不重複的登入 user 數，anonymous 是匿名連線數
func SetViewers(members, anonymous int) {
	websocketViewers.WithLabelValues("member").Set(float64(members))
	websocketViewers.WithLabelValues("anonymous").Set(float64(anonymous))
}

// resync frame 送出一次（PubSub 重建後或 reconfirm）
func WSResync() {
	websocketResyncTotal.Inc()
}

// PubSub 聾掉被判定並重建
func PubSubDeaf() {
	pubSubDeafTotal.Inc()
}

// 已確認的房間又收到訂閱確認（go-redis 重連或 SUBSCRIBE 重送）
func PubSubReconfirmed() {
	pubSubReconfirmTotal.Inc()
}

// PubSub 重建後，某房間重新訂閱確認花的時間
func PubSubResubscribed(d time.Duration) {
	pubSubResubscribeDuration.Observe(d.Seconds())
}
