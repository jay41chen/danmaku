package httpx

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
)

// 跨 package 共用的 HTTP helper（handler、middleware 都會用）

// 所有回 JSON 的 handler 都走這裡
// logger 由呼叫端傳入（照 checkRate 慣例）：各 handler 的 component 不同，log 才分得出是哪來的
func WriteJSON(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, v any) {
	// marshal 在 WriteHeader 之前做，失敗時還能改回 500；這是程式寫錯，記 Error
	data, err := json.Marshal(v)
	if err != nil {
		logger.ErrorContext(r.Context(), "marshal json response failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// header 要在 WriteHeader 之前設，之後設會被忽略
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// 跟原本 json.Encoder.Encode 的輸出一樣（Encode 會在最後補換行）
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		// 通常是 client 已經斷線；status 已經送出，只能記 log
		logger.WarnContext(r.Context(), "write json response failed", "error", err)
	}
}

// 限制 body 大小（1 MB）再 decode JSON 到 dst；dst 要傳 pointer
// 包在一起是為了不會忘記限制：直接寫 json.NewDecoder(r.Body) 的 handler 就是漏了
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(dst)
}

// ClientIP 回傳用來做限流的 client IP
// trustedHeaderKey 是要信任的 header 名稱（例如 "X-Real-IP"）：
//   - 只有在前面有我們自己的 proxy 會「覆蓋」這個 header 時才能設定，否則 client 可以偽造
//   - 空字串代表不信任任何 header，只用 TCP 連線對端的位址
func ClientIP(r *http.Request, trustedHeaderKey string) string {
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

// 所有「沒登入」的 401 都走這裡，body 和 header 完全一樣：
// RequireAuth 的各種失敗（沒帶 / scheme 錯 / token 壞 / 過期）要讓 client 無從分辨原因，
// handler 遇到「token 有效但 user 已被刪除」也回同一份，client 只需要一種處理方式（去 /refresh）
// WWW-Authenticate 是 RFC 6750 規定 401 要帶的，告訴 client 要用 Bearer
func WriteUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
