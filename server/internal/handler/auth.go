package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/store"
)

const (
	// login 失敗回 400 而不是 401，讓 401 只代表「access token 失效」，client 的 refresh interceptor 不用排除 /login
	// 防帳號列舉靠的是所有失敗回應一模一樣，不是 status code
	msgInvalidCredentials = "invalid username or password"
)

var (
	dummyHash, _ = auth.HashPassword("timing-equalizer")
)

// 限流規則（機制在 ratelimit.go）
var (
	loginIPRule       = rateRule{"login:ip", 20, time.Minute}            // 一台機器狂試很多帳號
	loginUsernameRule = rateRule{"login:username", 10, 15 * time.Minute} // 很多 IP 一起猜同一個帳號
	registerIPRule    = rateRule{"register:ip", 5, time.Hour}            // 一台機器狂註冊垃圾帳號
	refreshIPRule     = rateRule{"refresh:ip", 10, time.Minute}          // 一台機器狂猜 refresh token
)

const (
	refreshTokenTTL = 7 * 24 * time.Hour
	// 只蓋「回應掉了、client 自動重送」的最壞情況：timeout 5 秒 × 重試 3 次（Step 10）
	refreshTokenGrace = 15 * time.Second
)

type AuthHandler struct {
	db               store.DBStoreInterface
	logger           *slog.Logger
	limiter          store.RateLimiterInterface
	trustedHeaderKey string // 要信任的 client IP header；空字串 = 只用 RemoteAddr（見 clientIP）
	tokens           tokenIssuer
}

// handler 只需要「能簽 access token」：定義在使用方，*auth.TokenIssuer 不用宣告就自動符合
// test 可以換成一定會失敗的 fake，驗證簽失敗時不會 Reset / Refund
type tokenIssuer interface {
	IssueAccessToken(userID int64) (token string, expiresIn time.Duration, err error)
}

func NewAuthHandler(db store.DBStoreInterface, limiter store.RateLimiterInterface, trustedHeaderKey string, tokens tokenIssuer) *AuthHandler {
	return &AuthHandler{
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
// 檢查username
// 檢查password
// 檢查hash
// 試著創造user
// 回傳結果
func (this *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	// 做ip的429 check（在 bcrypt 之前擋，不讓攻擊者讓 server 做白工）
	if !this.allow(w, r, registerIPRule, httpx.ClientIP(r, this.trustedHeaderKey)) {
		return
	}

	var req credentials
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := auth.ValidateUsername(req.Username); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := auth.ValidatePassword(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// hash 後面還要用，所以不能寫成 if hash, err := ...; 那樣 hash 只活在 if 裡面
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		// 只記 err，不能記 req.Password
		this.logger.ErrorContext(r.Context(), "hash password failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	id, err := this.db.CreateUser(r.Context(), req.Username, hash)
	// 先比對已知的 sentinel，剩下的才是真的系統錯誤
	if errors.Is(err, store.ErrUsernameTaken) {
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "create user failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// header 要在 WriteHeader 之前設，之後設會被忽略
	httpx.SetContentType(w)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(userResponse{ID: id, Username: req.Username})
}

func (this *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// 做ip的429 check
	// ip 存起來：登入成功時要用同一個 key 退還
	// 放在最前面：被擋的 IP 連 body 都不用 decode；格式錯誤的請求也算這個 IP 的一次失敗（不 Refund）
	ip := httpx.ClientIP(r, this.trustedHeaderKey)
	if !this.allow(w, r, loginIPRule, ip) {
		return
	}

	var req credentials
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// 格式規則是公開的（register 會回 400 告知），格式不合法的帳號不可能存在，所以直接回、不洩漏帳號是否存在
	// 放在 username 限流之前：不讓超長字串變成 Redis key；也不跑 bcrypt、不查 DB
	if err := auth.ValidateUsername(req.Username); err != nil {
		http.Error(w, msgInvalidCredentials, http.StatusBadRequest)
		return
	}

	// 做username的429 check
	// 轉小寫：GetUser 不分大小寫，key 也要統一，不然換大小寫（Jay / JAY）就能多猜
	username := strings.ToLower(req.Username)
	if !this.allow(w, r, loginUsernameRule, username) {
		return
	}

	// 查詢user在不在
	user, err := this.db.GetUser(r.Context(), req.Username)
	if errors.Is(err, store.ErrUserNotFound) {
		// 結果不重要，目的是花跟「密碼錯」一樣的時間
		// 借以避免讓攻擊者知道錯誤
		_, _ = auth.CheckPassword(dummyHash, req.Password)
		http.Error(w, msgInvalidCredentials, http.StatusBadRequest)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get user failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 有user，檢查密碼是不是正確的
	success, err := auth.CheckPassword(user.PasswordHash, req.Password)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "check password failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !success {
		http.Error(w, msgInvalidCredentials, http.StatusBadRequest)
		return
	}

	// 先簽 token：簽失敗是 server 的錯（500），這次不算「登入成功」，下面的 Reset / Refund 都不做
	tokens, err := this.issueTokens(r, user.ID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 成功登入，reset 這個帳號的計數（使用者自己打錯幾次不該被鎖）
	// 失敗只記 log：限流的附屬功能壞掉，不該讓登入失敗
	if err := this.limiter.Reset(r.Context(), loginUsernameRule.key(username)); err != nil {
		this.logger.WarnContext(r.Context(), "rate limit reset failed",
			"rule", loginUsernameRule.name, "error", err)
	}
	// IP 只退還這一次（-1），等於只數失敗：共用出口 IP 的人（公司 NAT）正常登入不會互相擋到
	// 不 reset IP：否則攻擊者用自己的帳號登入一次，就能把 IP 計數歸零
	if err := this.limiter.Refund(r.Context(), loginIPRule.key(ip)); err != nil {
		this.logger.WarnContext(r.Context(), "rate limit refund failed",
			"rule", loginIPRule.name, "error", err)
	}

	httpx.SetContentType(w)
	w.WriteHeader(http.StatusOK)
	// 用 DB 存的 username：用 jay 登入，註冊時存的是 Jay，前端要顯示 Jay
	json.NewEncoder(w).Encode(loginResponse{
		tokenResponse: tokens,
		User:          userResponse{ID: user.ID, Username: user.Username},
	})
}

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
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	newPlain, newHash := auth.NewRefreshToken()
	userID, err := this.db.RotateRefreshToken(r.Context(),
		auth.HashRefreshToken(req.RefreshToken), newHash, refreshTokenTTL, refreshTokenGrace)
	// invalid 和 duplicate use 對 client 都是 401（重新登入），不讓對方知道「這個 token 曾經有效」
	if errors.Is(err, store.ErrRefreshTokenInvalid) {
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}
	var dupUse *store.RefreshTokenDuplicateUseError
	if errors.As(err, &dupUse) {
		// 資安事件：記 Warn 方便追查是哪個帳號被盜、從哪來；不記 token 本身
		// IP 用 httpx.ClientIP：RemoteAddr 在 nginx 後面只會是 nginx 的 IP
		this.logger.WarnContext(r.Context(), "refresh token duplicate use detected, revoked all tokens of user",
			"user_id", dupUse.UserID, "client_ip", ip)
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "rotate refresh token failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// rotate 已經 Commit 了：這裡失敗的話 client 拿舊的重送，會落在 grace 內再換一次，不會被登出
	tokens, err := this.withAccessToken(r, userID, newPlain)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	httpx.SetContentType(w)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(tokens)
}

// 只作廢這一個 refresh token（這台裝置登出）
// 查不到 / 已經作廢都回 204：logout 是 idempotent，也不透露 token 存不存在
// access token 不處理：它是 stateless 的，最多再活到 exp（1 小時），client 自己丟掉
func (this *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil || req.RefreshToken == "" {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := this.db.RevokeRefreshToken(r.Context(), auth.HashRefreshToken(req.RefreshToken)); err != nil {
		// DB 壞了要回 500：回 204 的話 client 以為登出了，token 其實還有效
		this.logger.ErrorContext(r.Context(), "revoke refresh token failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
