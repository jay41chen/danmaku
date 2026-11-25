package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/jay41chen/danmaku/server/internal/httpx"
)

// panic可以視為exception
// 這個consumer的功用可以視為
// 在每個request上加上try catch，
// 以免一個出錯的request就讓server shut down
func RecoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// 為了知道header是否已經被寫過，需要用custom recorder
			// 暴露出狀態
			writer := NewStatusCodeRecorder(w)
			defer func() {
				rec := recover()
				switch {
				case rec == http.ErrAbortHandler:
					// 類似rethrow，讓外層handle 這個panic
					panic(rec)
				case rec != nil:
					// 印出stacktrace方便查bug
					slog.ErrorContext(r.Context(), "panic recovered", "panic", rec, "method", r.Method, "path", r.URL.Path, "stacktrace", string(debug.Stack()))
					if !writer.Committed() {
						// 如果response還沒寫入才要寫進去
						// 已送出時 status 改不了，再寫只會讓 net/http 印 "superfluous WriteHeader"；
						// WebSocket（已 Hijack）則是 "WriteHeader on hijacked connection"
						httpx.WriteError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
					}
				}
			}()

			next.ServeHTTP(writer, r)
		})
}
