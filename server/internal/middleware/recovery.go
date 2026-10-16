package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
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
				rec := recover()
				switch {
				case rec == http.ErrAbortHandler:
					// 類似rethrow，讓外層handle 這個panic
					panic(rec)
				case rec != nil:
					// 印出stacktrace方便查bug
					slog.ErrorContext(r.Context(), "panic recovered", "panic", rec, "method", r.Method, "path", r.URL.Path, "stacktrace", string(debug.Stack()))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
}
