package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jay41chen/danmaku/server/internal/httpx"
)

// handler 只需要「能驗 access token」：*auth.TokenIssuer 不用宣告就自動符合
type AccessTokenParser interface {
	ParseAccessToken(token string) (int64, error)
}

// context key 用自己的 type：別的 package 的 key 就算字串一樣也不會撞
type userIDKey struct{}

// RequireAuth 驗 Authorization: Bearer <access token>，通過才進 next，並把 user id 放進 context
// 失敗一律用 httpx.WriteUnauthorized
func RequireAuth(tokens AccessTokenParser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := bearerToken(r)
			if err != nil {
				httpx.WriteUnauthorized(w)
				return
			}
			userID, err := tokens.ParseAccessToken(token)
			if err != nil {
				httpx.WriteUnauthorized(w)
				return
			}
			ctx := withUserID(r.Context(), userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("missing Authorization header")
	}

	// The scheme is case-insensitive per RFC 6750 / RFC 7235
	const prefix = "bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", errors.New("authorization header is not a Bearer token")
	}

	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	return token, nil
}

func withUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// 取出 RequireAuth 放進去的 user id；沒經過 RequireAuth 的 request 回 (0, false)
func UserIDFromContext(ctx context.Context) (int64, bool) {
	value := ctx.Value(userIDKey{})
	id, ok := value.(int64)
	if !ok {
		return 0, false
	}
	return id, ok
}
