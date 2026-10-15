package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

type StatusCodeRecorder struct {
	http.ResponseWriter
	statusCode int
}

// constructor
func NewStatusCodeRecorder(w http.ResponseWriter) *StatusCodeRecorder {
	return &StatusCodeRecorder{ResponseWriter: w, statusCode: 200}
}

func (r *StatusCodeRecorder) StatusCode() int {
	return r.statusCode
}

func (r *StatusCodeRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

// WebSocket upgrade 需要 Hijack 取得底層 TCP 連線
// 所以需要override這個method，否則upgrade的request會failed
func (r *StatusCodeRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

func (r *StatusCodeRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
