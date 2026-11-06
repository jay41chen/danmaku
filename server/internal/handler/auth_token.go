package handler

import (
	"errors"
	"net/http"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// login 用：簽 access token + 存一個新的 refresh token
// 失敗時已經記了 log，呼叫端只要回 500
func (this *AuthHandler) issueTokens(r *http.Request, userID int64) (tokenResponse, error) {
	plain, hash := auth.NewRefreshToken()
	if err := this.db.CreateRefreshToken(r.Context(), userID, hash, refreshTokenTTL); err != nil {
		this.logger.ErrorContext(r.Context(), "create refresh token failed", "error", err)
		return tokenResponse{}, err
	}
	return this.withAccessToken(r, userID, plain)
}

// 簽 access token，跟已經存好的 refresh token 明文組成 response
func (this *AuthHandler) withAccessToken(r *http.Request, userID int64, refreshToken string) (tokenResponse, error) {
	accessToken, expiresIn, err := this.tokens.IssueAccessToken(userID)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "issue access token failed", "error", err)
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(expiresIn.Seconds()),
		RefreshToken: refreshToken,
	}, nil
}

// 用 refresh token 換一組新的（rotation：舊的作廢，回傳新的 refresh token）
func (this *AuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	ip := httpx.ClientIP(r, this.trustedHeaderKey)
	if !this.allow(w, r, refreshIPRule, ip) {
		return
	}

	var req refreshRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	newPlain, newHash := auth.NewRefreshToken()
	userID, err := this.db.RotateRefreshToken(r.Context(),
		auth.HashRefreshToken(req.RefreshToken), newHash, refreshTokenTTL, refreshTokenGrace)
	if err != nil {
		this.writeRotateError(w, r, err, ip)
		return
	}

	// rotate 已經 Commit 了：這裡失敗的話 client 拿舊的重送，會落在 grace 內再換一次，不會被登出
	tokens, err := this.withAccessToken(r, userID, newPlain)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, tokens)
}

// 只在 RotateRefreshToken 回 err 時呼叫
func (this *AuthHandler) writeRotateError(w http.ResponseWriter, r *http.Request, err error, ip string) {
	// invalid 和 duplicate use 對 client 都是 401（重新登入），不讓對方知道「這個 token 曾經有效」
	// RefreshTokenRevokedError 一定要在 errors.Is(Invalid) 之前判斷：它的 Is 也會對 Invalid 回 true，
	// 順序反過來的話會被 Is 先接走，log 就不會記
	var revoked *model.RefreshTokenRevokedError
	if errors.As(err, &revoked) {
		// 被撤銷的憑證又出現（多半是攻擊者手上的舊 token）；不記 token 本身
		this.logger.WarnContext(r.Context(), "refresh token issued before credentials revoked was used",
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
		this.logger.WarnContext(r.Context(), "refresh token duplicate use detected, revoked all tokens of user",
			"user_id", dupUse.UserID, "client_ip", ip)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "invalid refresh token")
		return
	}
	this.logger.ErrorContext(r.Context(), "rotate refresh token failed", "error", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// 只作廢這一個 refresh token（這台裝置登出）
// 查不到 / 已經作廢都回 204：logout 是 idempotent，也不透露 token 存不存在
// access token 不處理：它是 stateless 的，最多再活到 exp（1 小時），client 自己丟掉
func (this *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	if err := this.db.RevokeRefreshToken(r.Context(), auth.HashRefreshToken(req.RefreshToken)); err != nil {
		// DB 壞了要回 500：回 204 的話 client 以為登出了，token 其實還有效
		this.logger.ErrorContext(r.Context(), "revoke refresh token failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
