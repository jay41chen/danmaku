package store

import (
	"strings"
	"testing"
)

// RunMigration 的 error 會被 main 用 slog 印進 log，所以不管哪一步失敗，都不能帶出密碼。
// 所有 case 都連到 127.0.0.1:1（沒有服務），不需要真的 DB：不是在 parse 就是在連線時失敗。
func TestRunMigrationDoesNotLeakPassword(t *testing.T) {
	// 密碼用 s3kr1t（不會自然出現在錯誤訊息裡；sec 會撞到 second、secure）：url.Parse 的錯誤會帶出密碼前段（":s3k"）或 escape 片段（"%zz"）
	leaks := []string{"s3kr1t", "s3k", "%zz"}

	cases := []struct {
		name string
		raw  string
	}{
		// 密碼本身讓 URL 格式壞掉（特殊字元沒有 percent-encode）
		{"# in password", "postgres://u:s3k#r1t@127.0.0.1:1/db"},
		{"/ in password", "postgres://u:s3k/r1t@127.0.0.1:1/db"},
		{"? in password", "postgres://u:s3k?r1t@127.0.0.1:1/db"},
		{"bad escape in password", "postgres://u:s3k%zzr1t@127.0.0.1:1/db"},

		// 密碼正常，但其他部分壞掉
		{"invalid port", "postgres://u:s3kr1t@127.0.0.1:port/db"},
		{"unsupported scheme", "mysql://u:s3kr1t@127.0.0.1:1/db"},

		// 格式正確，連線失敗
		{"url unreachable", "postgres://u:s3kr1t@127.0.0.1:1/db"},
		{"dsn unreachable", "host=127.0.0.1 port=1 user=u password=s3kr1t"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given: tc.raw

			// When
			err := RunMigration(tc.raw)

			// Then
			if err == nil {
				t.Fatalf("RunMigration(%q) error = nil, want error", tc.raw)
			}
			for _, s := range leaks {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error leaks password (contains %q): %v", s, err)
				}
			}
		})
	}
}

// 已知限制：密碼裡沒 encode 的 @ 會讓 URL 被切錯（parser 分不出哪個 @ 才是分隔），
// 密碼片段可能被當成 host / database 印進錯誤訊息，所以這裡只確認會回 error，不檢查洩漏。
// 避免方式是把密碼的特殊字元 percent-encode（@ → %40），README 的 Environment Variables 有寫。
func TestRunMigrationUnencodedAtInPassword(t *testing.T) {
	// Given
	raw := "postgres://u:s3k@r1t@127.0.0.1:1/db"

	// When
	err := RunMigration(raw)

	// Then
	if err == nil {
		t.Fatalf("RunMigration(%q) error = nil, want error", raw)
	}
}
