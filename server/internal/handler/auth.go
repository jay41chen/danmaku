package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/ratelimit"
)

const (
	// login 失敗回 400 而不是 401，讓 401 只代表「access token 失效」，client 的 refresh interceptor 不用排除 /login
	// 防帳號列舉靠的是所有失敗回應一模一樣，不是 status code
	msgInvalidCredentials = "invalid username or password"
)

// 限流規則（機制在 internal/ratelimit）；login / register 的規則在 use case
var (
	refreshIPRule = ratelimit.Rule{Name: "refresh:ip", Limit: 10, Window: time.Minute} // 一台機器狂猜 refresh token
)

// 只留 refresh 的寬限期；refresh token 的有效期（usecase.RefreshTokenTTL）由 use case 擁有
const (
	// 只蓋「回應掉了、client 自動重送」的最壞情況：timeout 5 秒 × 重試 3 次（Step 10）
	refreshTokenGrace = 15 * time.Second
)

// handler 需要的 DB 方法：interface 定義在使用方（Go 的 accept interfaces, return structs）
// 只列這個 handler 真的會呼叫的方法；*pgstore.DBStore 不用宣告就自動符合，main.go 照樣傳同一個 db
// test 換成 fake 時，只要實作這幾個方法
type authStore interface {
	GetUserByID(ctx context.Context, id int64) (model.User, error)

	RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error)
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
}

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 authStore）
type authUseCase interface {
	CheckRegisterIP(ctx context.Context, ip string) error
	Register(ctx context.Context, username, password string) (int64, error)
	CheckLoginIP(ctx context.Context, ip string) error
	Login(ctx context.Context, ip, username, password string) (model.User, model.TokenPair, error)
}

type AuthHandler struct {
	auth             authUseCase
	db               authStore
	logger           *slog.Logger
	limiter          rateLimiter
	trustedHeaderKey string // 要信任的 client IP header；空字串 = 只用 RemoteAddr（見 httpx.ClientIP）
	tokens           tokenIssuer
}

// handler 只需要「能簽 access token」：定義在使用方，*auth.TokenIssuer 不用宣告就自動符合
// test 可以換成一定會失敗的 fake，驗證簽失敗時不會 Reset / Refund
type tokenIssuer interface {
	IssueAccessToken(userID int64) (token string, expiresIn time.Duration, err error)
}

func NewAuthHandler(auth authUseCase, db authStore, limiter rateLimiter, trustedHeaderKey string, tokens tokenIssuer) *AuthHandler {
	return &AuthHandler{
		auth:             auth,
		db:               db,
		logger:           slog.Default().With("component", "auth"),
		limiter:          limiter,
		trustedHeaderKey: trustedHeaderKey,
		tokens:           tokens,
	}
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
	if err := this.auth.CheckRegisterIP(r.Context(), httpx.ClientIP(r, this.trustedHeaderKey)); err != nil {
		var limited *model.RateLimitedError
		if errors.As(err, &limited) {
			writeTooManyRequests(w, limited.RetryAfter)
			return
		}
		// CheckRegisterIP 只會回 RateLimitedError；萬一不是，當系統錯誤
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
	ip := httpx.ClientIP(r, this.trustedHeaderKey)
	if err := this.auth.CheckLoginIP(r.Context(), ip); err != nil {
		var limited *model.RateLimitedError
		if errors.As(err, &limited) {
			writeTooManyRequests(w, limited.RetryAfter)
			return
		}
		// CheckLoginIP 只會回 RateLimitedError；萬一不是，當系統錯誤
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
	var limited *model.RateLimitedError
	switch {
	case errors.As(err, &limited):
		writeTooManyRequests(w, limited.RetryAfter)
		return
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

	user, err := this.db.GetUserByID(r.Context(), userID)
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
