package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/store"
)

const (
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

// login 成功的 response：欄位命名照 OAuth 2.0（RFC 6749 §5.1）
// expires_in 是相對秒數：client 不用解 JWT、也不受手機時鐘誤差影響
type loginResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int          `json:"expires_in"`
	User        userResponse `json:"user"`
}

// flow:
// 檢查username
// 檢查password
// 檢查hash
// 試著創造user
// 回傳結果
func (this *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodPost}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 做ip的429 check（在 bcrypt 之前擋，不讓攻擊者讓 server 做白工）
	if !this.allow(w, r, registerIPRule, clientIP(r, this.trustedHeaderKey)) {
		return
	}

	var req credentials
	if err := decodeJSONBody(w, r, &req); err != nil {
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
	setContentType(w)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(userResponse{ID: id, Username: req.Username})
}

func (this *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodPost}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 做ip的429 check
	// ip 存起來：登入成功時要用同一個 key 退還
	ip := clientIP(r, this.trustedHeaderKey)
	if !this.allow(w, r, loginIPRule, ip) {
		return
	}

	var req credentials
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
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
		http.Error(w, msgInvalidCredentials, http.StatusUnauthorized)
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
		http.Error(w, msgInvalidCredentials, http.StatusUnauthorized)
		return
	}

	// 先簽 token：簽失敗是 server 的錯（500），這次不算「登入成功」，下面的 Reset / Refund 都不做
	accessToken, expiresIn, err := this.tokens.IssueAccessToken(user.ID)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "issue access token failed", "error", err)
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

	setContentType(w)
	w.WriteHeader(http.StatusOK)
	// 用 DB 存的 username：用 jay 登入，註冊時存的是 Jay，前端要顯示 Jay
	json.NewEncoder(w).Encode(loginResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(expiresIn.Seconds()),
		User:        userResponse{ID: user.ID, Username: user.Username},
	})
}
