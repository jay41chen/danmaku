package prometheus

import (
	"net/http"
	"strconv"
	"time"

	promlib "github.com/prometheus/client_golang/prometheus"
)

func HttpMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, statusCode: 200}
		start := time.Now()

		next.ServeHTTP(recorder, r)

		duration := time.Since(start)

		HttpRequestCount.With(promlib.Labels{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status_code": strconv.Itoa(recorder.statusCode),
		}).Inc()
		HttpRequestDuration.With(promlib.Labels{
			"method": r.Method,
			"path":   r.URL.Path,
		}).Observe(duration.Seconds())
	})
}
