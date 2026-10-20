package auth

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{"min length 3", strings.Repeat("a", 3), false},
		{"max length 32", strings.Repeat("a", 32), false},
		{"letters digits underscore", "Jay_41", false},
		{"too short 2", strings.Repeat("a", 2), true},
		{"too long 33", strings.Repeat("a", 33), true},
		{"empty", "", true},
		{"contains space", "jay chen", true},
		{"contains hyphen", "jay-chen", true},
		{"non-ASCII", "酷拉皮卡丘", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUsername(tc.username)

			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateUsername(%q) error = %v, wantErr %v", tc.username, err, tc.wantErr)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
	}{
		// 不用 "aaaaaaaa"：它在常見密碼清單裡，會被黑名單擋掉，就測不到長度邊界
		{"min length 8", "kq7vz2mx", false},
		{"max length 72", strings.Repeat("a", 72), false},
		{"symbols and spaces", "P@ss w0rd!#$%", false},
		// 6 個字但 18 byte：長度以 byte 計
		{"non-ASCII 18 bytes", "密碼密碼密碼", false},
		{"too short 7", strings.Repeat("a", 7), true},
		{"too long 73", strings.Repeat("a", 73), true},
		{"empty", "", true},
		// 25 個字但 75 byte：用字元數算會通過，但 bcrypt 只收 72 byte
		{"non-ASCII 75 bytes", strings.Repeat("密", 25), true},
		{"not common passphrase", "correct horse battery", false},
		// 長度合格，但在常見密碼清單裡
		{"common password", "12345678", true},
		// 清單存小寫，大小寫變化也要擋
		{"common password with different case", "PassWord1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)

			if (err != nil) != tc.wantErr {
				t.Errorf("ValidatePassword(%q) error = %v, wantErr %v", tc.password, err, tc.wantErr)
			}
		})
	}
}
