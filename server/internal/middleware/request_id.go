package middleware

import (
	"context"
	"crypto/rand"
	"net/http"
	"regexp"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// 檢查request上有沒有帶`request id`
			// 沒有或不合法的話要建立一個新的
			reqID := r.Header.Get(RequestIDHeader)
			if !isValidRequestID(reqID) {
				reqID = generateRequestID()
			}
			// requestID
			// 1. 寫給response
			// 2. 透過context把value留給後面的middleware
			w.Header().Set(RequestIDHeader, reqID)

			ctx := withRequestID(r.Context(), reqID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestIDFromContext(ctx context.Context) string {
	value := ctx.Value(requestIDKey{})
	str, _ := value.(string)
	return str
}

func generateRequestID() string {
	return rand.Text()
}

func isValidRequestID(id string) bool {
	return requestIDPattern.MatchString(id)
}
