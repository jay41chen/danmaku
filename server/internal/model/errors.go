package model

// 這裡是 store 實作回傳、handler 用 errors.Is / errors.As 判斷的 domain 結果
// store 負責把 DB 專屬的錯誤（pgx.ErrNoRows、SQLSTATE）翻成這些，caller 不需要知道底層用哪種 DB

import (
	"errors"
	"fmt"
)

// --- user ---
var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrUserNotFound  = errors.New("user not found")
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
// 不再觸發撤銷（見 store/refresh_token.go 的 tokenState 上方的狀態表），但要讓 handler 記 log，所以獨立成帶 userID 的 error
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
