// Package testenv 給 integration test 讀環境變數：
// 本機沒設就 skip（不用每次都開 DB），CI 上沒設就 fail（不准「全綠但什麼都沒測」）
package testenv

import (
	"os"
	"testing"
)

// Require 回傳環境變數 name 的值；沒設時在 CI（CI 環境變數非空，GitHub Actions 會自動設）上 Fatal，否則 Skip
func Require(t testing.TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v != "" {
		return v
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s not set in CI; integration tests must not be skipped", name)
	}
	t.Skipf("%s not set; skipping integration test", name)
	return ""
}
