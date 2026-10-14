package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestCount = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total",
			Help: "Total number of HTTP requests"},
		[]string{"method", "path", "status_code"},
	)
	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Druation for each HTTP request",
		},
		[]string{"method", "path"},
	)
)

func PrometheusMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ResponseWriter只能寫header，無法取值
		// 所以只好自己弄一個struct把status code存下來做比對
		// 這邊的200是default value，真實的值在WritterHeader時
		// 被assign成正確的value
		recorder := &StatusCodeRecorder{ResponseWriter: w, statusCode: 200}
		start := time.Now()
		next.ServeHTTP(recorder, r)
		duration := time.Since(start)

		requestCount.With(prometheus.Labels{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status_code": strconv.Itoa(recorder.statusCode),
		}).Inc()
		requestDuration.With(prometheus.Labels{
			"method": r.Method,
			"path":   r.URL.Path,
		}).Observe(duration.Seconds())

	})
}
