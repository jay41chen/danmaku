package middleware

import (
	"log/slog"
	"net/http"
)

// panic可以視為exception
// 這個consumer的功用可以視為
// 在每個request上加上try catch，
// 以免一個出錯的request就讓server shut down
func RecoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// check recovery
			defer func() {
				if panic := recover(); panic != nil {
					slog.Error("panic recovered", "panic", panic, "method", r.Method, "path", r.URL.Path)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
}
