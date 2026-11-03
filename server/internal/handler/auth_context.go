package handler

import (
	"log/slog"
	"net/http"

	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
)

// 照 ratelimit.go 的 checkRate 慣例：logger 由呼叫端傳入（每個 handler 的 component 不同）
// 回傳 false = 已經回了 500，handler 直接 return（跟 allow() 一樣的約定）
func requireUserID(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (int64, bool) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		// server 設定錯誤、不是 client 的錯：這個 route 忘了用 get/post 註冊（誤用 publicGet/publicPost）
		logger.ErrorContext(r.Context(), "no user id in context, route must be registered with get/post, not publicGet/publicPost")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return 0, false
	}
	return userID, true
}
