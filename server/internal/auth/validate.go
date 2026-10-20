package auth

import (
	"errors"
	"regexp"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

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
	return nil
}
