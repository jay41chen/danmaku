package prometheus

import (
	"net/http"
	"strconv"
	"time"

	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	promlib "github.com/prometheus/client_golang/prometheus"
)

func HttpMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := middleware.NewStatusCodeRecorder(w)
		start := time.Now()

		next.ServeHTTP(recorder, r)

		duration := time.Since(start)

		HttpRequestCount.With(promlib.Labels{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status_code": strconv.Itoa(recorder.StatusCode()),
		}).Inc()
		HttpRequestDuration.With(promlib.Labels{
			"method": r.Method,
			"path":   r.URL.Path,
		}).Observe(duration.Seconds())
	})
}
