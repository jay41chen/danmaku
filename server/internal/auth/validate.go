package auth

import (
	"fmt"
	"regexp"
	"strings"

	_ "embed" // 底線 import：沒有直接用到 embed 這個 package，但 go:embed 需要它
	"github.com/jay41chen/danmaku/server/internal/model"
)

const (
	// HS256 secret 的最短長度（256 bits）。config 啟動時檢查、NewTokenIssuer 也檢查，共用這個數字
	MinSecretLength = 32
)

var (
	//go:embed common_passwords.txt
	commonPasswordsRaw string

	// 常見密碼清單（SecLists 10k-most-common，MIT），只留 8–72 byte、已轉小寫
	// 用途：擋 password spraying（拿常見密碼對所有帳號各試一次）
	commonPasswords = func() map[string]struct{} {
		set := map[string]struct{}{}
		for line := range strings.SplitSeq(commonPasswordsRaw, "\n") {
			if line != "" {
				set[line] = struct{}{}
			}
		}
		return set
	}()

	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)
)

// 驗證失敗回傳的 sentinel：帶 code，handler 用 httpx.WriteCodedError 直接寫成錯誤回應
var (
	errUsernameInvalid   = &model.CodedError{Code: "username_invalid", Message: "username must be 3-32 characters of letters, digits or _"}
	errPasswordLength    = &model.CodedError{Code: "password_length", Message: "password must be 8-72 bytes"}
	errPasswordTooCommon = &model.CodedError{Code: "password_too_common", Message: "password is too common"}
)

// 只允許3~32位的英數；失敗回 errUsernameInvalid
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return errUsernameInvalid
	}
	return nil
}

// 只允許8~72byte的任意輸入；長度不符回 errPasswordLength，太常見回 errPasswordTooCommon
func ValidatePassword(password string) error {
	if len(password) < 8 || len(password) > 72 {
		return errPasswordLength
	}
	if _, inList := commonPasswords[strings.ToLower(password)]; inList {
		return errPasswordTooCommon
	}
	return nil
}

func ValidateJWTSecretLength(jwtSecret string) error {
	if l := len(jwtSecret); l < MinSecretLength {
		return fmt.Errorf("jwt secret needs at least %d bytes, got %d", MinSecretLength, l)
	}
	return nil
}
