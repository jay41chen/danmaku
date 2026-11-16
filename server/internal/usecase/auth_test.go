package usecase

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 認證 use case 的假 store
type fakeAuthStore struct {
	user      model.User
	getErr    error
	rotateErr error // RotateRefreshToken 回傳的錯
}

func (f *fakeAuthStore) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	return 0, nil
}

func (f *fakeAuthStore) GetUser(ctx context.Context, username string) (model.User, error) {
	return f.user, f.getErr
}

func (f *fakeAuthStore) CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	return nil
}

func (f *fakeAuthStore) GetUserByID(ctx context.Context, id int64) (model.User, error) {
	return f.user, f.getErr
}

func (f *fakeAuthStore) RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error) {
	return 7, f.rotateErr
}

func (f *fakeAuthStore) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	return nil
}

// 假 limiter：blockKey 收到就擋；Reset / Refund 只記錄
type fakeAuthLimiter struct {
	blockKey   string
	retryAfter time.Duration
}

func (f *fakeAuthLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if key == f.blockKey {
		return false, f.retryAfter, nil
	}
	return true, 0, nil
}

func (f *fakeAuthLimiter) Reset(ctx context.Context, key string) error  { return nil }
func (f *fakeAuthLimiter) Refund(ctx context.Context, key string) error { return nil }

func newTestAuthTokens(t *testing.T) *auth.TokenIssuer {
	t.Helper()
	issuer, err := auth.NewTokenIssuer(strings.Repeat("k", 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func TestAuthLoginChecksPasswordForTiming(t *testing.T) {
	const password = "kq7vz2mx"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	stored := model.User{ID: 7, Username: "Jay", PasswordHash: hash}

	// 未知帳號也必須真的跑一次 bcrypt（用 dummyHash），否則回應時間會洩漏帳號存不存在
	// 用 spy 取代 checkPassword 確認呼叫次數與收到的 hash；回應則照舊
	t.Run("checkPassword is called once for unknown user with dummyHash", func(t *testing.T) {
		uc := NewAuthUseCase(&fakeAuthStore{getErr: model.ErrUserNotFound}, &fakeAuthLimiter{}, newTestAuthTokens(t))
		var gotHashes []string
		uc.checkPassword = func(hash, pw string) (bool, error) {
			gotHashes = append(gotHashes, hash)
			return false, nil
		}

		_, _, err := uc.Login(context.Background(), "1.2.3.4", "nobody", password)

		if !errors.Is(err, model.ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
		if len(gotHashes) != 1 || gotHashes[0] != dummyHash {
			t.Errorf("checkPassword hashes = %q, want exactly [dummyHash]", gotHashes)
		}
	})

	t.Run("checkPassword is called once for wrong password with user's hash", func(t *testing.T) {
		uc := NewAuthUseCase(&fakeAuthStore{user: stored}, &fakeAuthLimiter{}, newTestAuthTokens(t))
		var gotHashes []string
		uc.checkPassword = func(hash, pw string) (bool, error) {
			gotHashes = append(gotHashes, hash)
			return false, nil
		}

		_, _, err := uc.Login(context.Background(), "1.2.3.4", "jay", "wrong-password")

		if !errors.Is(err, model.ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
		if len(gotHashes) != 1 || gotHashes[0] != stored.PasswordHash {
			t.Errorf("checkPassword hashes = %q, want exactly [user's PasswordHash]", gotHashes)
		}
	})
}

// 守住帳號限流被擋時 log 要帶 client_ip，才查得到攻擊來源 IP；也要回 RetryAfter 給 handler 翻成 429
func TestAuthLoginUsernameBlockedLogsClientIP(t *testing.T) {
	limiter := &fakeAuthLimiter{blockKey: LoginUsernameRule.Key("jay"), retryAfter: 30 * time.Second}
	uc := NewAuthUseCase(&fakeAuthStore{}, limiter, newTestAuthTokens(t))
	var buf bytes.Buffer
	uc.logger = slog.New(slog.NewTextHandler(&buf, nil))

	_, _, err := uc.Login(context.Background(), "1.2.3.4", "Jay", "kq7vz2mx")

	var limited *model.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *model.RateLimitedError", err)
	}
	if limited.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", limited.RetryAfter)
	}
	if !strings.Contains(buf.String(), "client_ip=1.2.3.4") {
		t.Errorf("log %q does not contain client_ip=1.2.3.4", buf.String())
	}
}

// 守住 refresh 被 IP 限流擋住時 log 要帶 client_ip；也要回 RetryAfter 給 handler 翻成 429
func TestAuthCheckRefreshIPBlockedLogsClientIP(t *testing.T) {
	limiter := &fakeAuthLimiter{blockKey: RefreshIPRule.Key("1.2.3.4"), retryAfter: 30 * time.Second}
	uc := NewAuthUseCase(&fakeAuthStore{}, limiter, newTestAuthTokens(t))
	var buf bytes.Buffer
	uc.logger = slog.New(slog.NewTextHandler(&buf, nil))

	err := uc.CheckRefreshIP(context.Background(), "1.2.3.4")

	var limited *model.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *model.RateLimitedError", err)
	}
	if limited.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", limited.RetryAfter)
	}
	if !strings.Contains(buf.String(), "client_ip=1.2.3.4") {
		t.Errorf("log %q does not contain client_ip=1.2.3.4", buf.String())
	}
}

// Refresh 把 rotate 的錯帶步驟名稱包起來，但 handler 靠 errors.As 決定 401 和 log，包了之後仍要認得出來
func TestAuthRefreshWrapsButPreservesRotateError(t *testing.T) {
	uc := NewAuthUseCase(&fakeAuthStore{rotateErr: &model.RefreshTokenRevokedError{UserID: 7}}, &fakeAuthLimiter{}, newTestAuthTokens(t))

	_, err := uc.Refresh(context.Background(), "some-token")

	var revoked *model.RefreshTokenRevokedError
	if !errors.As(err, &revoked) || revoked.UserID != 7 {
		t.Fatalf("err = %v, want it to wrap *model.RefreshTokenRevokedError{UserID: 7}", err)
	}
	if !strings.Contains(err.Error(), "rotate refresh token") {
		t.Errorf("err = %q, want it to name the failed step", err.Error())
	}
}
