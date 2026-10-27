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
)
