package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 跨 package 共用的 HTTP helper（handler、middleware 都會用）

// 錯誤回應裡的內容，HTTP 錯誤回應和 WS 的 error frame 共用這個型別
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 錯誤回應的外層，輸出 {"error": {"code": ..., "message": ...}}；只有 WriteError 用，所以不 export
type errorResponse struct {
	Error ErrorDetail `json:"error"`
}

// 所有回 JSON 的 handler 都走這裡
// logger 由呼叫端傳入（照 checkRate 慣例）：各 handler 的 component 不同，log 才分得出是哪來的
func WriteJSON(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, v any) {
	// marshal 在 WriteHeader 之前做，失敗時還能改回 500；這是程式寫錯，記 Error
	data, err := json.Marshal(v)
	if err != nil {
		logger.ErrorContext(r.Context(), "marshal json response failed", "error", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
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
	WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
}

// 寫出統一格式的 HTTP 錯誤，所有 handler 和 middleware 都走這裡
// 用途：取代 http.Error；不收 logger，因為 WriteUnauthorized 和 panic recovery 手上沒有
func WriteError(w http.ResponseWriter, status int, code, message string) {
	// 錯誤回應一律不快取，不管先前成功路徑設了什麼：GET messages 會先設 s-maxage=1 才寫 body，
	// 之後 marshal 失敗或 panic 走到這裡，要覆蓋掉才不會讓 CDN / 共用快取存起這個錯誤；
	// /me 和 /me/rooms 則是刻意一開始就設 no-store，這裡的值與它們一致
	w.Header().Set("Cache-Control", "no-store")
	// http.Error 原本就會設 nosniff，換成 WriteError 後維持這個行為
	w.Header().Set("X-Content-Type-Options", "nosniff")
	errResponse := errorResponse{Error: ErrorDetail{
		Code:    code,
		Message: message,
	}}
	// marshal 在 WriteHeader 之前做，失敗時還能改回 500；欄位都是字串，正常不會失敗
	b, err := json.Marshal(errResponse)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// header 要在 WriteHeader 之前設，之後設會被忽略
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b = append(b, '\n')
	// 寫入失敗通常是 client 已經斷線；status 已經送出，也沒有 logger 可記，只能忽略
	_, _ = w.Write(b)
}

// 把驗證函式回傳的 error 寫成錯誤回應，文字來自 CodedError，不會洩漏其他內部錯誤
// 用途：取代 http.Error(w, err.Error(), status)；logger 由呼叫端傳入（照 WriteJSON 慣例）
func WriteCodedError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, err error) {
	var coded *model.CodedError
	// errors.As 會沿著 %w 往內找；找到就回 true，並把結果放進 coded
	if errors.As(err, &coded) {
		WriteError(w, status, coded.Code, coded.Message)
		return
	}
	// 沒有 code 的 error 不該傳到這裡，是呼叫端寫錯；真正原因只進 log，內容可能有敏感資訊，不能回給 client
	logger.ErrorContext(r.Context(), "error without code passed to WriteCodedError", "error", err)
	WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}
