package model

// 這裡是 store / use case 回傳、handler 用 errors.Is / errors.As 判斷的 domain 結果
// store 負責把 DB 專屬的錯誤（pgx.ErrNoRows、SQLSTATE）翻成這些，caller 不需要知道底層用哪種 DB

import (
	"errors"
	"fmt"
	"time"
)

// --- user ---
var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrUserNotFound  = errors.New("user not found")
	// login 的帳號格式不合法、不存在、密碼錯都回這個（防帳號列舉：對外要一模一樣）
	ErrInvalidCredentials = errors.New("invalid credentials")
)

// --- message ---
var (
	// user 已被刪除，但他的 access token 還沒過期（stateless，撤不掉）
	ErrMessageUserNotFound = errors.New("message user not found")
)

// --- room ---
var (
	// 查不到、或已 soft delete：對外都當成沒這個房間
	ErrRoomNotFound = errors.New("room not found")
	// 這個 user 的未刪除房間已達上限
	ErrRoomLimitReached = errors.New("room limit reached")
	// owner 已被刪除，但他的 access token 還沒過期（同 ErrMessageUserNotFound 的情況）
	ErrRoomOwnerNotFound = errors.New("room owner not found")
	// 房間列表的 limit 不在 1..MaxRoomListLimit：handler 應該先擋掉，走到這裡代表 caller 有 bug
	ErrInvalidLimit = errors.New("invalid limit")
	// 熱門房間排行（Redis）讀取失敗：handler 翻成 503；真正的錯誤用 %w 包在裡面，log 看得到
	ErrViewerRankingUnavailable = errors.New("viewer ranking unavailable")
	// 操作者不是房主或房管
	ErrRoomForbidden = errors.New("room forbidden")
	// 操作者有權限，但不能動這個對象
	ErrRoomTargetForbidden = errors.New("room target forbidden")
	// 要先解除 ban 才能指派
	ErrRoomRoleConflict = errors.New("room role conflict")
	// desired 不是可指派的角色（見 RoomRole.Assignable）；PUT 只收哪些值是 handler 的 API 合約
	ErrInvalidRole = errors.New("invalid role")
)

// --- refresh token ---
var (
	// 查不到、已過期、撤銷全部之前發的（valid_after）、或 user 已刪除：當作沒這個 token
	ErrRefreshTokenInvalid = errors.New("refresh token invalid")
	// 已作廢超過 grace period 又出現：視為外洩，這個 user 的 token 已全部撤銷
	// 業界叫 refresh token reuse detection（RFC 9700、Auth0）；這裡的 reuse 是「重複使用」，不是「復用」
	// RotateRefreshToken 實際回的是 *RefreshTokenDuplicateUseError；只想知道「是不是 duplicate use」用 errors.Is 比這個就好
	ErrRefreshTokenDuplicateUse = errors.New("refresh token used twice")
)

// 帶著 userID 的 duplicate use error（同 pgconn.PgError 的模式：error 本身帶資料，用 errors.As 取出）
// 不用「err 之外再回 userID」：Go 的 caller 預設 err != nil 時其他回傳值是零值，不會去看
type RefreshTokenDuplicateUseError struct {
	UserID int64 // 被撤銷全部 token 的 user
}

func (e *RefreshTokenDuplicateUseError) Error() string {
	return fmt.Sprintf("refresh token used twice, user %d", e.UserID)
}

// errors.Is 會呼叫這個：讓 errors.Is(err, ErrRefreshTokenDuplicateUse) 繼續成立
// 用 == 比是因為 sentinel error 比的是「同一個變數」（指標），不是訊息內容：
// 兩個 errors.New("refresh token used twice") 用 == 比是 false
func (e *RefreshTokenDuplicateUseError) Is(target error) bool {
	return target == ErrRefreshTokenDuplicateUse
}

// 被撤銷的憑證（valid_after 之前發的 token）又被拿來用：資安訊號（多半是攻擊者手上的舊 token）
// 不再觸發撤銷（見 pgstore/refresh_token.go 的 tokenState 上方的狀態表），但要讓 handler 記 log，所以獨立成帶 userID 的 error
// errors.Is(err, ErrRefreshTokenInvalid) 仍成立：對 client 仍是 invalid
type RefreshTokenRevokedError struct {
	UserID int64
}

func (e *RefreshTokenRevokedError) Error() string {
	return fmt.Sprintf("refresh token issued before credentials revoked, user %d", e.UserID)
}

func (e *RefreshTokenRevokedError) Is(target error) bool {
	return target == ErrRefreshTokenInvalid
}

// --- coded error ---

// 帶穩定 code 的錯誤：Code 給程式判斷、給 client 當翻譯 key，Message 是後備文字
// 用途：驗證錯誤自己帶著 code，handler 不用再對應一次；這裡不放 HTTP status，由 handler 決定
// Code 發布後就是 API 合約，不能改名；Message 會原樣回給 client，不能放內部資訊
type CodedError struct {
	Code    string
	Message string
}

// 實作 error 介面（Go 只比對 method 名稱和簽名），讓它可以當一般 error 回傳，也能被 errors.Is / errors.As 比對
func (this *CodedError) Error() string {
	return this.Message
}

// 限流規則擋住時，use case 回傳這個；handler 翻成 429 + Retry-After
// RetryAfter 是「所有被擋的規則裡最長的等待時間」，只回短的 client 馬上重試又會被另一條擋
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (this *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited, retry after %v", this.RetryAfter)
}
