package auth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	_ "embed" // 底線 import：沒有直接用到 embed 這個 package，但 go:embed 需要它
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

// 只允許3~32位的英數
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("username must be 3-32 characters of letters, digits or _")
	}
	return nil
}

// 只允許8~72byte的任意輸入
func ValidatePassword(password string) error {
	if len(password) < 8 || len(password) > 72 {
		return errors.New("password must be 8-72 bytes")
	}
	if _, inList := commonPasswords[strings.ToLower(password)]; inList {
		return errors.New("password is too common")
	}
	return nil
}

func ValidateJWTSecretLength(jwtSecret string) error {
	if l := len(jwtSecret); l < MinSecretLength {
		return fmt.Errorf("jwt secret needs at least %d bytes, got %d", MinSecretLength, l)
	}
	return nil
}
