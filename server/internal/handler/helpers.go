package handler

import (
	"encoding/json"
	"net"
	"net/http"
	"slices"
)

// handler package 共用的 HTTP helper（各個 Handler 都會用）

func setContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func checkMethodValidation(r *http.Request, allowMethods []string) bool {
	return slices.Contains(allowMethods, r.Method)
}

// 限制 body 大小（1 MB）再 decode JSON 到 dst；dst 要傳 pointer
// 包在一起是為了不會忘記限制：直接寫 json.NewDecoder(r.Body) 的 handler 就是漏了
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(dst)
}

// clientIP 回傳用來做限流的 client IP
// trustedHeaderKey 是要信任的 header 名稱（例如 "X-Real-IP"）：
//   - 只有在前面有我們自己的 proxy 會「覆蓋」這個 header 時才能設定，否則 client 可以偽造
//   - 空字串代表不信任任何 header，只用 TCP 連線對端的位址
func clientIP(r *http.Request, trustedHeaderKey string) string {
	if trustedHeaderKey != "" {
		// 先以parse header的值為主
		if ip := net.ParseIP(r.Header.Get(trustedHeaderKey)); ip != nil {
			return ip.String()
		}
	}
	// 沒設定要信任的 header，或 header 沒帶 / 不是合法 IP：改用 TCP 連線對端的位址
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
