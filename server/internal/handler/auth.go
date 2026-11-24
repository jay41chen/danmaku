package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

const (
	// login 失敗回 400 而不是 401，讓 401 只代表「access token 失效」，client 的 refresh interceptor 不用排除 /login
	// 防帳號列舉靠的是所有失敗回應一模一樣，不是 status code
	msgInvalidCredentials = "invalid username or password"
)

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 ADR 0009）
type authUseCase interface {
	CheckRegisterIP(ctx context.Context, ip string) error
	Register(ctx context.Context, username, password string) (int64, error)
	CheckLoginIP(ctx context.Context, ip string) error
	Login(ctx context.Context, ip, username, password string) (model.User, model.TokenPair, error)
	CheckRefreshIP(ctx context.Context, ip string) error
	Refresh(ctx context.Context, refreshToken string) (model.TokenPair, error)
	Logout(ctx context.Context, refreshToken string) error
	CurrentUser(ctx context.Context, userID int64) (model.User, error)
}

type AuthHandler struct {
	auth             authUseCase
	logger           *slog.Logger
	trustedHeaderKey string      // 要信任的 client IP header；空字串 = 只用 RemoteAddr（見 httpx.ClientIP）
	clientIPWarned   atomic.Bool // client IP header 用不了的 warn log 是否已經印過（每個 process 只印一次）
}

func NewAuthHandler(auth authUseCase, trustedHeaderKey string) *AuthHandler {
	return &AuthHandler{
		auth:             auth,
		logger:           slog.Default().With("component", "auth"),
		trustedHeaderKey: trustedHeaderKey,
	}
}

// 取得限流用的 client IP；有設定 header 卻用不了時留下痕跡：
// 每次都 +1 metric（label 只有 reason，不會爆）；warn log 每個 process 只印一次，
// 因為設定錯誤時攻擊者可以一直送壞的 header 來洗 log
// log 裡的 header 值要截到 64 bytes：server 沒設 MaxHeaderBytes（Go 預設 1 MB），原樣印會被拿來灌 log
func (this *AuthHandler) clientIP(r *http.Request) string {
	ip, fallback := httpx.ClientIP(r, this.trustedHeaderKey)
	if fallback != "" {
		prom.ClientIPFallback(fallback)
		if this.clientIPWarned.CompareAndSwap(false, true) {
			value := r.Header.Get(this.trustedHeaderKey)
			if len(value) > 64 {
				value = value[:64]
			}
			this.logger.WarnContext(r.Context(), "client ip header unusable, falling back to RemoteAddr",
				"header", this.trustedHeaderKey, "reason", fallback, "value", value)
		}
	}
	return ip
}

// register / login 共用的 request body
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// 回給 client 的 user：只放能公開的欄位（不含 hash）
type userResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// login / refresh 成功都回這組：欄位命名照 OAuth 2.0（RFC 6749 §5.1）
// expires_in 是 access token 的相對秒數：client 不用解 JWT、也不受手機時鐘誤差影響
// refresh_token 是明文，只在這裡出現一次：DB 只存 hash，log 也不能印
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// embedding：JSON 會攤平成同一層（{"access_token":..., "user":{...}}），不是包一層 tokenResponse
type loginResponse struct {
	tokenResponse
	User userResponse `json:"user"`
}

// /refresh、/logout 的 request body
// 先放 JSON body；要不要改成 HttpOnly cookie 在 Step 10 決定
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// flow:
// 問 IP 限流（use case）
// 解析 body
// 交給 use case（檢查帳密格式、hash、建立 user）
// 回 201
func (this *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	// 做ip的429 check（在 bcrypt 之前擋，不讓攻擊者讓 server 做白工）
	if err := this.auth.CheckRegisterIP(r.Context(), this.clientIP(r)); err != nil {
		if writeIfRateLimited(w, err) {
			return
		}
		// CheckRegisterIP 只會回 RateLimitedError 或 ErrRateLimiterUnavailable；萬一不是，當系統錯誤
		this.logger.ErrorContext(r.Context(), "register failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	var req credentials
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	id, err := this.auth.Register(r.Context(), req.Username, req.Password)
	var coded *model.CodedError
	switch {
	case errors.As(err, &coded):
		httpx.WriteCodedError(w, r, this.logger, http.StatusBadRequest, coded)
		return
	case errors.Is(err, model.ErrUsernameTaken):
		httpx.WriteError(w, http.StatusConflict, "username_taken", "username already taken")
		return
	case err != nil:
		// 只記 err，不能記 req.Password（use case 的 error 也不含密碼）
		this.logger.ErrorContext(r.Context(), "register failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, userResponse{ID: id, Username: req.Username})
}

// flow:
// 問 IP 限流（use case；ip 存起來，登入成功時 use case 用同一個 key 退還）
// 解析 body
// 交給 use case（格式、帳號限流、驗密碼、簽 token、調整限流計數）
// 回 200
func (this *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// 放在最前面：被擋的 IP 連 body 都不用 decode
	ip := this.clientIP(r)
	if err := this.auth.CheckLoginIP(r.Context(), ip); err != nil {
		if writeIfRateLimited(w, err) {
			return
		}
		// CheckLoginIP 只會回 RateLimitedError 或 ErrRateLimiterUnavailable；萬一不是，當系統錯誤
		this.logger.ErrorContext(r.Context(), "login failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	var req credentials
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	user, pair, err := this.auth.Login(r.Context(), ip, req.Username, req.Password)
	if writeIfRateLimited(w, err) {
		return
	}
	switch {
	case errors.Is(err, model.ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_credentials", msgInvalidCredentials)
		return
	case err != nil:
		this.logger.ErrorContext(r.Context(), "login failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// 用 DB 存的 username：用 jay 登入，註冊時存的是 Jay，前端要顯示 Jay
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, loginResponse{
		tokenResponse: newTokenResponse(pair),
		User:          userResponse{ID: user.ID, Username: user.Username},
	})
}

// 把 use case 發的 TokenPair 轉成回給 client 的格式（login / refresh 共用）
func newTokenResponse(pair model.TokenPair) tokenResponse {
	return tokenResponse{
		AccessToken:  pair.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(pair.ExpiresIn.Seconds()),
		RefreshToken: pair.RefreshToken,
	}
}

// 回目前登入的 user：client 重新整理頁面（F5）後只剩 token，用這個取回身分
// 要登入的 route（用 get 註冊），user id 由 RequireAuth 放進 context
func (this *AuthHandler) HandleMe(w http.ResponseWriter, r *http.Request) {
	// 回應內容依「誰在問」（Authorization）而不同，不能進任何快取：
	// CDN 命中快取時不會經過 server 的驗證，會把 A 的資料給 B；
	// 瀏覽器也不存，避免同一台電腦換人登入時看到上一個人
	// 放在最前面：錯誤回應也不該被快取
	w.Header().Set("Cache-Control", "no-store")

	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}

	user, err := this.auth.CurrentUser(r.Context(), userID)
	if errors.Is(err, model.ErrUserNotFound) {
		// user 已刪除但 access token 還沒過期：跟 RequireAuth 同一個 401，
		// client 會去 /refresh → refresh token 已被 CASCADE 刪除 → 401 → 登出
		httpx.WriteUnauthorized(w)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get user by id failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, userResponse{ID: user.ID, Username: user.Username})
}
