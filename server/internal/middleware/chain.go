package middleware

import (
	"net/http"
)

type Middleware func(http.Handler) http.Handler

// 類似 Python decorator：第一個參數在最外圈
// 所以從最後一個開始包（最內圈先包）
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		handler = mw(handler)
	}
	return handler
}
