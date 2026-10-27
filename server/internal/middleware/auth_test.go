package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
)

// 假 parser：token → user id，查不到就當作驗證失敗（模擬過期 / 被竄改）
type fakeParser map[string]int64

func (f fakeParser) ParseAccessToken(token string) (int64, error) {
	id, ok := f[token]
	if !ok {
		return 0, errors.New("invalid token")
	}
	return id, nil
}

// RequireAuth 還是 stub 時會 panic：recover 成 test 失敗，不然整個 package 的 test 都會被中斷
func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
		}
	}()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequireAuthRejects(t *testing.T) {
	cases := []struct {
		name   string
		header string // 空字串 = 不帶 Authorization header
	}{
		{"no header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bearer without token", "Bearer"},
		{"bearer with empty token", "Bearer "},
		{"parser rejects token", "Bearer expired-or-tampered"},
	}

	var firstBody string
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
			h := RequireAuth(fakeParser{"good": 7})(next)
			req := httptest.NewRequest(http.MethodPost, "/messages", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			rec := serve(t, h, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if called {
				t.Error("next was called, want blocked")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
			// 所有 401 body 相同：client 分辨不出是哪一種失敗
			if i == 0 {
				firstBody = rec.Body.String()
			} else if rec.Body.String() != firstBody {
				t.Errorf("body = %q, want same as first case %q", rec.Body.String(), firstBody)
			}
		})
	}
}

func TestRequireAuthPasses(t *testing.T) {
	var gotID int64
	var gotOK bool
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotID, gotOK = UserIDFromContext(r.Context())
	})
	h := RequireAuth(fakeParser{"good": 7})(next)
	req := httptest.NewRequest(http.MethodPost, "/messages", nil)
	req.Header.Set("Authorization", "Bearer good")

	rec := serve(t, h, req)

	if !called {
		t.Fatalf("next not called, status = %d", rec.Code)
	}
	if !gotOK || gotID != 7 {
		t.Errorf("UserIDFromContext = (%d, %v), want (7, true)", gotID, gotOK)
	}
}

// 用真的 TokenIssuer 走一輪：簽出來的 token 能過、竄改過的不能過
// middleware 沒有 import cycle 問題（internal/auth 不依賴 middleware）
func TestRequireAuthRealTokenRoundTrip(t *testing.T) {
	issuer, err := auth.NewTokenIssuer("local-dev-only-jwt-secret-0123456789", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.IssueAccessToken(42)
	if err != nil {
		t.Fatal(err)
	}

	var gotID int64
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = UserIDFromContext(r.Context())
	})
	h := RequireAuth(issuer)(next)

	t.Run("valid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/messages", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rec := serve(t, h, req)

		if !gotOK || gotID != 42 {
			t.Errorf("UserIDFromContext = (%d, %v), want (42, true); status = %d", gotID, gotOK, rec.Code)
		}
	})

	t.Run("tampered token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/messages", nil)
		req.Header.Set("Authorization", "Bearer "+token+"x")

		rec := serve(t, h, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}

func TestUserIDFromContextEmpty(t *testing.T) {
	id, ok := UserIDFromContext(context.Background())
	if id != 0 || ok {
		t.Errorf("UserIDFromContext(Background) = (%d, %v), want (0, false)", id, ok)
	}
}
