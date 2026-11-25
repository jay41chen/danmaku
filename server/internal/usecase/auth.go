package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/ratelimit"
)

// 限流規則（機制在 internal/ratelimit）
// 匯出：規則由 use case 擁有，其他 package（例如 handler 測試）只讀
var (
	LoginIPRule       = ratelimit.Rule{Name: "login:ip", Limit: 20, Window: time.Minute, FailClosed: true}            // 一台機器狂試很多帳號
	LoginUsernameRule = ratelimit.Rule{Name: "login:username", Limit: 10, Window: 15 * time.Minute, FailClosed: true} // 很多 IP 一起猜同一個帳號
	RegisterIPRule    = ratelimit.Rule{Name: "register:ip", Limit: 5, Window: time.Hour, FailClosed: true}            // 一台機器狂註冊垃圾帳號
	RefreshIPRule     = ratelimit.Rule{Name: "refresh:ip", Limit: 10, Window: time.Minute}                            // 一台機器狂猜 refresh token
)

const (
	// refresh token 的有效期（login 發新的、refresh rotate 時都用同一個值）
	refreshTokenTTL = 7 * 24 * time.Hour
	// 只蓋「回應掉了、client 自動重送」的最壞情況：timeout 5 秒 × 重試 3 次（Step 10）
	refreshTokenGrace = 15 * time.Second
)

var dummyHash = mustDummyHash()

// 算不出 dummyHash 的話，login 的 timing 防護會悄悄失效（查無使用者時少了一次雜湊的時間，可以拿來猜帳號是否存在），
// 所以在 package init 就 panic，不要等到上線後才發現
func mustDummyHash() string {
	h, err := auth.HashPassword("timing-equalizer")
	if err != nil {
		panic("usecase: compute dummy password hash: " + err.Error())
	}
	return h
}

// 這個 use case 需要的 DB 方法（consumer-side interface，同 ADR 0009）
type authStore interface {
	CreateUser(ctx context.Context, username string, passwordHash string) (int64, error)
	GetUser(ctx context.Context, username string) (model.User, error)
	CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	GetUserByID(ctx context.Context, id int64) (model.User, error)
	RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error)
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
}

// 限流器：Allow 之外，login 成功時還要 Reset / Refund（說明見 ratelimit 各 interface）
type authLimiter interface {
	ratelimit.Limiter
	ratelimit.Resetter
	ratelimit.Refunder
}

// 只需要「能簽 access token」：*auth.TokenIssuer 不用宣告就自動符合
type tokenIssuer interface {
	IssueAccessToken(userID int64) (token string, expiresIn time.Duration, err error)
}

// 帳號的業務操作（註冊、登入、refresh、登出、查目前 user）：不知道 HTTP 和 SQL
type AuthUseCase struct {
	store   authStore
	limiter authLimiter
	tokens  tokenIssuer
	logger  *slog.Logger
	// 可替換只為了 test：要能確認「未知帳號」那條路也真的跑了 bcrypt，
	// 否則 timing 防帳號列舉沒有回歸保護。正式環境永遠是 auth.CheckPassword
	checkPassword func(hash, password string) (bool, error)
}

// constructor
func NewAuthUseCase(store authStore, limiter authLimiter, tokens tokenIssuer) *AuthUseCase {
	return &AuthUseCase{
		store:         store,
		limiter:       limiter,
		tokens:        tokens,
		logger:        slog.Default().With("component", "auth"),
		checkPassword: auth.CheckPassword,
	}
}

// 註冊前先問這個 IP 的限流；被擋回 *model.RateLimitedError；limiter 壞掉且規則 fail closed 回 model.ErrRateLimiterUnavailable；否則 nil
// 在 bcrypt 之前擋，不讓攻擊者讓 server 做白工；用途：POST /register 最前面（decode body 之前）
func (u *AuthUseCase) CheckRegisterIP(ctx context.Context, ip string) error {
	return u.checkIP(ctx, RegisterIPRule, ip)
}

// 登入前先問這個 IP 的限流；被擋回 *model.RateLimitedError；limiter 壞掉且規則 fail closed 回 model.ErrRateLimiterUnavailable；否則 nil
// 放在 decode body 之前：被擋的 IP 連 body 都不用 decode；格式錯誤的請求也算這個 IP 的一次失敗（不 Refund）
// 用途：POST /login 最前面
func (u *AuthUseCase) CheckLoginIP(ctx context.Context, ip string) error {
	return u.checkIP(ctx, LoginIPRule, ip)
}

// refresh 前先問這個 IP 的限流；被擋回 *model.RateLimitedError，否則 nil（refresh 規則 fail open：limiter 壞掉時放行，已登入的人不受 Redis 影響）
// 放在 decode body 之前，理由同 CheckLoginIP；用途：POST /refresh 最前面
func (u *AuthUseCase) CheckRefreshIP(ctx context.Context, ip string) error {
	return u.checkIP(ctx, RefreshIPRule, ip)
}

// 問 IP 規則放不放行（limiter 壞掉時照規則 fail open / fail closed，後者回 model.ErrRateLimiterUnavailable）；log 帶 client_ip（HTTP 層的 RemoteAddr 在 nginx 後面只會是 nginx 的 IP）
func (u *AuthUseCase) checkIP(ctx context.Context, rule ratelimit.Rule, ip string) error {
	allowed, retryAfter, err := ratelimit.Check(ctx, u.limiter, u.logger, rule, ip, "client_ip", ip)
	if err != nil {
		return err
	}
	if !allowed {
		return &model.RateLimitedError{RetryAfter: retryAfter}
	}
	return nil
}

// 註冊：檢查 username → 檢查 password → hash → 建立 user，回新 user 的 id
// 格式不合法回 *model.CodedError；帳號已存在回 model.ErrUsernameTaken；其他錯誤帶步驟名稱包起來
// 不把 password 放進任何 error 或 log
// 用途：POST /register
func (u *AuthUseCase) Register(ctx context.Context, username, password string) (int64, error) {
	if err := auth.ValidateUsername(username); err != nil {
		return 0, err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return 0, err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return 0, fmt.Errorf("hash password: %w", err)
	}

	id, err := u.store.CreateUser(ctx, username, hash)
	// 已知的 sentinel 原樣回，handler 翻成 409；剩下的才是真的系統錯誤
	if errors.Is(err, model.ErrUsernameTaken) {
		return 0, err
	}
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// 登入：回 DB 存的 user（用 jay 登入、註冊時存 Jay，前端要顯示 Jay）和一組新的 token
// 帳號格式不合法 / 不存在 / 密碼錯都回 model.ErrInvalidCredentials；被帳號限流擋住回 *model.RateLimitedError
// 其他錯誤帶步驟名稱包起來；ip 用來退還 IP 限流額度（IP 本身的限流在 CheckLoginIP）
// 用途：POST /login（CheckLoginIP 通過、decode body 之後）
func (u *AuthUseCase) Login(ctx context.Context, ip, username, password string) (model.User, model.TokenPair, error) {
	// 格式規則是公開的（register 會回 400 告知），格式不合法的帳號不可能存在，所以直接回、不洩漏帳號是否存在
	// 放在 username 限流之前：不讓超長字串變成 Redis key；也不跑 bcrypt、不查 DB
	if err := auth.ValidateUsername(username); err != nil {
		return model.User{}, model.TokenPair{}, model.ErrInvalidCredentials
	}

	// 轉小寫：GetUser 不分大小寫，key 也要統一，不然換大小寫（Jay / JAY）就能多猜
	lowerName := strings.ToLower(username)
	allowed, retryAfter, err := ratelimit.Check(ctx, u.limiter, u.logger, LoginUsernameRule, lowerName, "client_ip", ip)
	if err != nil {
		return model.User{}, model.TokenPair{}, err
	}
	if !allowed {
		return model.User{}, model.TokenPair{}, &model.RateLimitedError{RetryAfter: retryAfter}
	}

	user, err := u.store.GetUser(ctx, username)
	if errors.Is(err, model.ErrUserNotFound) {
		// 結果不重要，目的是花跟「密碼錯」一樣的時間
		// 借以避免讓攻擊者知道錯誤
		_, _ = u.checkPassword(dummyHash, password)
		return model.User{}, model.TokenPair{}, model.ErrInvalidCredentials
	}
	if err != nil {
		return model.User{}, model.TokenPair{}, fmt.Errorf("get user: %w", err)
	}

	// 有user，檢查密碼是不是正確的
	success, err := u.checkPassword(user.PasswordHash, password)
	if err != nil {
		return model.User{}, model.TokenPair{}, fmt.Errorf("check password: %w", err)
	}
	if !success {
		return model.User{}, model.TokenPair{}, model.ErrInvalidCredentials
	}

	// 先簽 token：簽失敗是 server 的錯（500），這次不算「登入成功」，下面的 Reset / Refund 都不做
	plain, hash := auth.NewRefreshToken()
	if err := u.store.CreateRefreshToken(ctx, user.ID, hash, refreshTokenTTL); err != nil {
		return model.User{}, model.TokenPair{}, fmt.Errorf("create refresh token: %w", err)
	}
	accessToken, expiresIn, err := u.tokens.IssueAccessToken(user.ID)
	if err != nil {
		return model.User{}, model.TokenPair{}, fmt.Errorf("issue access token: %w", err)
	}

	// 成功登入，reset 這個帳號的計數（使用者自己打錯幾次不該被鎖）
	// 失敗只記 log：限流的附屬功能壞掉，不該讓登入失敗
	// WithoutCancel：密碼是對的，這個事實不會因為 client 在收到回應前斷線而改變，計數照樣要清；
	// ratelimit.Reset / Refund 自己有 200ms 的預算，不會因此卡住
	cleanupCtx := context.WithoutCancel(ctx)
	ratelimit.Reset(cleanupCtx, u.limiter, u.logger, LoginUsernameRule, lowerName)
	// IP 只退還這一次（-1），等於只數失敗：共用出口 IP 的人（公司 NAT）正常登入不會互相擋到
	// 不 reset IP：否則攻擊者用自己的帳號登入一次，就能把 IP 計數歸零
	ratelimit.Refund(cleanupCtx, u.limiter, u.logger, LoginIPRule, ip)

	return user, model.TokenPair{
		AccessToken:  accessToken,
		ExpiresIn:    expiresIn,
		RefreshToken: plain,
	}, nil
}

// 用 refresh token 換一組新的（rotation：舊的作廢，回傳新的 refresh token）
// rotate 的錯誤帶步驟名稱包起來，errors.Is / As 仍可辨識 *model.RefreshTokenRevokedError、
// *model.RefreshTokenDuplicateUseError、model.ErrRefreshTokenInvalid（handler 靠它們決定 401 和 log）
// 不把 token 明文或 hash 放進任何 error 或 log；用途：POST /refresh（CheckRefreshIP 通過、decode body 之後）
func (u *AuthUseCase) Refresh(ctx context.Context, refreshToken string) (model.TokenPair, error) {
	newPlain, newHash := auth.NewRefreshToken()
	userID, err := u.store.RotateRefreshToken(ctx,
		auth.HashRefreshToken(refreshToken), newHash, refreshTokenTTL, refreshTokenGrace)
	if err != nil {
		return model.TokenPair{}, fmt.Errorf("rotate refresh token: %w", err)
	}

	// rotate 已經 Commit 了：這裡失敗的話 client 拿舊的重送，會落在 grace 內再換一次，不會被登出
	accessToken, expiresIn, err := u.tokens.IssueAccessToken(userID)
	if err != nil {
		return model.TokenPair{}, fmt.Errorf("issue access token: %w", err)
	}
	return model.TokenPair{
		AccessToken:  accessToken,
		ExpiresIn:    expiresIn,
		RefreshToken: newPlain,
	}, nil
}

// 只作廢這一個 refresh token（這台裝置登出）
// 查不到 / 已經作廢都不算錯（idempotent）；DB 失敗帶步驟名稱包起來回傳，不吞掉
// 不把 token 明文或 hash 放進任何 error 或 log；用途：POST /logout
func (u *AuthUseCase) Logout(ctx context.Context, refreshToken string) error {
	if err := u.store.RevokeRefreshToken(ctx, auth.HashRefreshToken(refreshToken)); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// 用 userID 查 user；查不到回 model.ErrUserNotFound（原樣回，不包）
// 用途：GET /me 和 WS auth frame（token 已由 RequireAuth / WS handler 解析，這裡只做 userID → user）
func (u *AuthUseCase) CurrentUser(ctx context.Context, userID int64) (model.User, error) {
	return u.store.GetUserByID(ctx, userID)
}
