package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func isURLSafeBase64(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '_'
}

func TestHashRefreshToken(t *testing.T) {
	// 已知答案：sha256("abc") 是標準測試向量（FIPS 180-2），確定真的是 SHA-256 + hex，不是別的算法或編碼
	// 答案是寫死的，所以也順便保證「同一個原文永遠算出同一個 hash」：/refresh 才查得到 DB 裡存的
	t.Run("matches known sha256 hex", func(t *testing.T) {
		want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

		if got := HashRefreshToken("abc"); got != want {
			t.Errorf("HashRefreshToken(%q) = %q, want %q", "abc", got, want)
		}
	})
}

func TestNewRefreshToken(t *testing.T) {
	t.Run("hash is the hash of plain", func(t *testing.T) {
		plain, hash := NewRefreshToken()

		if plain == "" {
			t.Fatal("plain is empty")
		}
		if want := HashRefreshToken(plain); hash != want {
			t.Errorf("hash = %q, want HashRefreshToken(plain) = %q", hash, want)
		}
		// DB 只能存 hash：原文不能出現在 hash 裡
		if hash == plain {
			t.Error("hash equals plain, want them different")
		}
	})

	// 32 bytes 的 base64url（無 padding）= 43 字元，而且只能有 A-Z a-z 0-9 - _
	// 換成 StdEncoding 會出現 + /、換成有 padding 的會多 =、bytes 數不對長度就不對
	t.Run("plain is 32 bytes of raw url base64", func(t *testing.T) {
		plain, _ := NewRefreshToken()

		if len(plain) != 43 {
			t.Errorf("len(plain) = %d, want 43", len(plain))
		}
		if i := strings.IndexFunc(plain, func(r rune) bool { return !isURLSafeBase64(r) }); i >= 0 {
			t.Errorf("plain = %q has %q at %d, want only A-Z a-z 0-9 - _", plain, plain[i], i)
		}
		b, err := base64.RawURLEncoding.DecodeString(plain)
		if err != nil || len(b) != 32 {
			t.Errorf("decode plain = (%d bytes, %v), want (32 bytes, nil)", len(b), err)
		}
	})

	// 不是真的測亂數品質，只抓「寫死回傳值」或「每次用同一個 seed」這種 bug
	t.Run("each call returns a different token", func(t *testing.T) {
		seen := map[string]bool{}
		for range 100 {
			plain, _ := NewRefreshToken()
			if seen[plain] {
				t.Fatalf("duplicate token %q", plain)
			}
			seen[plain] = true
		}
	})
}
