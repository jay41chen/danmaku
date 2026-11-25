package handler

import (
	"errors"
	"net/http"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 用 refresh token 換一組新的（rotation：舊的作廢，回傳新的 refresh token）
func (h *AuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	// 放在最前面：被擋的 IP 連 body 都不用 decode
	ip := h.clientIP(r)
	if err := h.auth.CheckRefreshIP(r.Context(), ip); err != nil {
		if writeIfRateLimited(w, err) {
			return
		}
		// CheckRefreshIP 只會回 RateLimitedError（refresh 規則 fail open）；萬一不是，當系統錯誤
		h.logger.ErrorContext(r.Context(), "refresh failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	var req refreshRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	pair, err := h.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		h.writeRotateError(w, r, err, ip)
		return
	}

	httpx.WriteJSON(w, r, h.logger, http.StatusOK, newTokenResponse(pair))
}

// 只在 Refresh 回 err 時呼叫
func (h *AuthHandler) writeRotateError(w http.ResponseWriter, r *http.Request, err error, ip string) {
	// invalid 和 duplicate use 對 client 都是 401（重新登入），不讓對方知道「這個 token 曾經有效」
	// RefreshTokenRevokedError 一定要在 errors.Is(Invalid) 之前判斷：它的 Is 也會對 Invalid 回 true，
	// 順序反過來的話會被 Is 先接走，log 就不會記
	var revoked *model.RefreshTokenRevokedError
	if errors.As(err, &revoked) {
		// 被撤銷的憑證又出現（多半是攻擊者手上的舊 token）；不記 token 本身
		h.logger.WarnContext(r.Context(), "refresh token issued before credentials revoked was used",
			"user_id", revoked.UserID, "client_ip", ip)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "invalid refresh token")
		return
	}
	if errors.Is(err, model.ErrRefreshTokenInvalid) {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "invalid refresh token")
		return
	}
	var dupUse *model.RefreshTokenDuplicateUseError
	if errors.As(err, &dupUse) {
		// 資安事件：記 Warn 方便追查是哪個帳號被盜、從哪來；不記 token 本身
		// IP 用 httpx.ClientIP：RemoteAddr 在 nginx 後面只會是 nginx 的 IP
		h.logger.WarnContext(r.Context(), "refresh token duplicate use detected, revoked all tokens of user",
			"user_id", dupUse.UserID, "client_ip", ip)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "invalid refresh token")
		return
	}
	h.logger.ErrorContext(r.Context(), "refresh failed", "error", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// 只作廢這一個 refresh token（這台裝置登出）
// 查不到 / 已經作廢都回 204：logout 是 idempotent，也不透露 token 存不存在
// access token 不處理：它是 stateless 的，最多再活到 exp（1 小時），client 自己丟掉
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	if err := h.auth.Logout(r.Context(), req.RefreshToken); err != nil {
		// DB 壞了要回 500：回 204 的話 client 以為登出了，token 其實還有效
		h.logger.ErrorContext(r.Context(), "logout failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
