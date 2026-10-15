package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return logging(next, logger.With("component", "http"))
	}
}

func logging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			// 這個endpoint直接略過
			next.ServeHTTP(w, r)
			return
		}
		// 記錄開始時間+寫log
		start := time.Now()
		recorder := NewStatusCodeRecorder(w)
		next.ServeHTTP(recorder, r)
		code := recorder.StatusCode()
		logLevel := determineLogLevel(code)
		logger.Log(r.Context(), logLevel, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", code,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func determineLogLevel(code int) slog.Level {
	switch {
	case code >= 500:
		return slog.LevelError
	case code >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
