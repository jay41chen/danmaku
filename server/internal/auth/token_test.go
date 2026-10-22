package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 剛好 32 bytes
const testSecret = "0123456789abcdef0123456789abcdef"

func TestNewTokenIssuer(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{"empty secret", "", true},
		{"31 bytes too short", testSecret[:31], true},
		{"32 bytes ok", testSecret, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTokenIssuer(tc.secret, time.Hour)

			if (err != nil) != tc.wantErr {
				t.Errorf("NewTokenIssuer(len %d) error = %v, wantErr %v", len(tc.secret), err, tc.wantErr)
			}
		})
	}
}

func TestIssueAndParseAccessToken(t *testing.T) {
	t.Run("round trip returns user id", func(t *testing.T) {
		issuer := newTestIssuer(t, testSecret, time.Hour)

		token, _, err := issuer.IssueAccessToken(7)
		if err != nil {
			t.Fatalf("IssueAccessToken(7) error = %v", err)
		}
		userID, err := issuer.ParseAccessToken(token)

		if err != nil || userID != 7 {
			t.Errorf("ParseAccessToken() = (%d, %v), want (7, nil)", userID, err)
		}
	})

	// 不靠 ParseAccessToken 自己檢查：直接用套件解開，看 claims 是不是照規格放
	t.Run("sets sub, iat and exp = iat + ttl", func(t *testing.T) {
		issuer := newTestIssuer(t, testSecret, time.Hour)
		token, expiresIn, err := issuer.IssueAccessToken(7)
		if err != nil {
			t.Fatalf("IssueAccessToken(7) error = %v", err)
		}
		if expiresIn != time.Hour {
			t.Errorf("expiresIn = %v, want %v", expiresIn, time.Hour)
		}

		claims := &jwt.RegisteredClaims{}
		if _, err := jwt.ParseWithClaims(token, claims, testKeyFunc); err != nil {
			t.Fatalf("parse issued token: %v", err)
		}

		if claims.Subject != "7" {
			t.Errorf("sub = %q, want %q", claims.Subject, "7")
		}
		if claims.IssuedAt == nil || claims.ExpiresAt == nil {
			t.Fatalf("iat = %v, exp = %v, want both set", claims.IssuedAt, claims.ExpiresAt)
		}
		if got := claims.ExpiresAt.Sub(claims.IssuedAt.Time); got != time.Hour {
			t.Errorf("exp - iat = %v, want %v", got, time.Hour)
		}
	})

	t.Run("rejects tampered payload", func(t *testing.T) {
		issuer := newTestIssuer(t, testSecret, time.Hour)
		token, _, err := issuer.IssueAccessToken(7) // 唯一一次簽章：這是 jay 拿到的真 token
		if err != nil {
			t.Fatalf("IssueAccessToken(7) error = %v", err)
		}

		// 以下都在模擬攻擊者：只能改字串，不能碰 secret
		parts := strings.Split(token, ".") // [header, payload, signature]
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		forged := strings.Replace(string(payload), `"sub":"7"`, `"sub":"1"`, 1)
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged)) // 編回 base64
		tampered := strings.Join(parts, ".")                            // 用 "." 接回去

		_, err = issuer.ParseAccessToken(tampered)

		if tampered == token { // 確定有改到我想改的地方，而不是改錯了，用原本的token比對
			t.Fatalf("payload not changed, check the Replace target: %s", payload)
		}
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("ParseAccessToken(tampered) error = %v, want ErrInvalidToken", err)
		}
	})
}

// 每一種「不該被接受」的 token，ParseAccessToken 都要回 ErrInvalidToken
func TestParseAccessTokenRejects(t *testing.T) {
	issuer := newTestIssuer(t, testSecret, time.Hour)
	now := time.Now()
	valid := jwt.RegisteredClaims{
		Subject:   "7",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}

	cases := []struct {
		name  string
		token string
	}{
		{"garbage", "not-a-jwt"},
		{"empty", ""},
		// 別的 server（或攻擊者）用另一把 secret 簽的
		{"wrong secret", sign(t, jwt.SigningMethodHS256, valid, []byte("ffffffffffffffffffffffffffffffff"))},
		// alg: none = 沒有簽章，誰都做得出來
		{"alg none", sign(t, jwt.SigningMethodNone, valid, jwt.UnsafeAllowNoneSignatureType)},
		// 同一把 secret、不同演算法：只接受 HS256（WithValidMethods）
		{"alg HS512", sign(t, jwt.SigningMethodHS512, valid, []byte(testSecret))},
		{"expired", sign(t, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject:   "7",
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
		}, []byte(testSecret))},
		// 沒有 exp = 永遠不過期，不能接受（WithExpirationRequired）
		{"missing exp", sign(t, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject:  "7",
			IssuedAt: jwt.NewNumericDate(now),
		}, []byte(testSecret))},
		// 簽章對，但 sub 不是數字：不能讓它變成 user 0
		{"non-numeric sub", sign(t, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject:   "jay",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		}, []byte(testSecret))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, err := issuer.ParseAccessToken(tc.token)

			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("ParseAccessToken() = (%d, %v), want ErrInvalidToken", userID, err)
			}
		})
	}
}

func newTestIssuer(t *testing.T, secret string, ttl time.Duration) *TokenIssuer {
	t.Helper()
	issuer, err := NewTokenIssuer(secret, ttl)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return issuer
}

// 測試自己簽 token：用來做出 IssueAccessToken 不會產生的各種壞 token
func sign(t *testing.T, method jwt.SigningMethod, claims jwt.RegisteredClaims, key any) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return token
}

func testKeyFunc(*jwt.Token) (any, error) {
	return []byte(testSecret), nil
}

var _ = strings.Split // TODO 那個 case 會用到；寫完後刪掉這行
