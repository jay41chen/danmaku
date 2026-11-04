package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
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

// handler 需要的 DB 方法：interface 定義在使用方（Go 的 accept interfaces, return structs）
// 只列這個 handler 真的會呼叫的方法；*store.DBStore 不用宣告就自動符合，main.go 照樣傳同一個 db
// test 換成 fake 時，只要實作這幾個方法
type authStore interface {
	CreateUser(ctx context.Context, username string, passwordHash string) (int64, error)
	GetUser(ctx context.Context, username string) (model.User, error)
	GetUserByID(ctx context.Context, id int64) (model.User, error)

	CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error)
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
}

type AuthHandler struct {
	db               authStore
	logger           *slog.Logger
	limiter          store.RateLimiterInterface
	trustedHeaderKey string // 要信任的 client IP header；空字串 = 只用 RemoteAddr（見 httpx.ClientIP）
	tokens           tokenIssuer
	// 可替換只為了 test：要能確認「未知帳號」那條路也真的跑了 bcrypt，
	// 否則 timing 防帳號列舉沒有回歸保護。正式環境永遠是 auth.CheckPassword
	checkPassword func(hash, password string) (bool, error)
}

// handler 只需要「能簽 access token」：定義在使用方，*auth.TokenIssuer 不用宣告就自動符合
// test 可以換成一定會失敗的 fake，驗證簽失敗時不會 Reset / Refund
type tokenIssuer interface {
	IssueAccessToken(userID int64) (token string, expiresIn time.Duration, err error)
}

func NewAuthHandler(db authStore, limiter store.RateLimiterInterface, trustedHeaderKey string, tokens tokenIssuer) *AuthHandler {
	return &AuthHandler{
		db:               db,
		logger:           slog.Default().With("component", "auth"),
		limiter:          limiter,
		trustedHeaderKey: trustedHeaderKey,
		tokens:           tokens,
		checkPassword:    auth.CheckPassword,
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

	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, userResponse{ID: id, Username: req.Username})
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
		_, _ = this.checkPassword(dummyHash, req.Password)
		http.Error(w, msgInvalidCredentials, http.StatusBadRequest)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get user failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 有user，檢查密碼是不是正確的
	success, err := this.checkPassword(user.PasswordHash, req.Password)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "check password failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !success {
		http.Error(w, msgInvalidCredentials, http.StatusBadRequest)
		return
	}

	tokens, err := this.completeLogin(r, user.ID, username, ip)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 用 DB 存的 username：用 jay 登入，註冊時存的是 Jay，前端要顯示 Jay
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, loginResponse{
		tokenResponse: tokens,
		User:          userResponse{ID: user.ID, Username: user.Username},
	})
}

// 密碼比對成功之後呼叫：簽 token + 調整限流計數
// username 是轉小寫的那個（限流 key 用的），不是 req.Username
// 失敗時已經記了 log，呼叫端只要回 500
func (this *AuthHandler) completeLogin(r *http.Request, userID int64, username, ip string) (tokenResponse, error) {
	// 先簽 token：簽失敗是 server 的錯（500），這次不算「登入成功」，下面的 Reset / Refund 都不做
	tokens, err := this.issueTokens(r, userID)
	if err != nil {
		return tokenResponse{}, err
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
	return tokens, nil
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
	if errors.Is(err, store.ErrUserNotFound) {
		// user 已刪除但 access token 還沒過期：跟 RequireAuth 同一個 401，
		// client 會去 /refresh → refresh token 已被 CASCADE 刪除 → 401 → 登出
		httpx.WriteUnauthorized(w)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get user by id failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, userResponse{ID: user.ID, Username: user.Username})
}
