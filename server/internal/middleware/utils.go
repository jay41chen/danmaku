package middleware

import (
	"net/http"
)

type StatusCodeRecorder struct {
	// embedding，類似繼承，所以現在
	// [StatusCodeRecorder]自動滿足了[http.ResponseWriter]
	// 的Write、Header、WriteHeader這三個method，同時可以
	// override
	http.ResponseWriter
	statusCode int
}

func (r *StatusCodeRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}
