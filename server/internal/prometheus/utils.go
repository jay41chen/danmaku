package prometheus

import (
	"bufio"
	"net"
	"net/http"
)

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// WebSocket upgrade 需要 Hijack 取得底層 TCP 連線
// 所以需要override這個method，否則upgrade的request會failed
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}
