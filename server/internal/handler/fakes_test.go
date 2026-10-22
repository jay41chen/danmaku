package handler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// handler test 共用的假 DB：只實作用得到的方法，其他的呼叫到會 nil panic
type fakeDBStore struct {
	store.DBStoreInterface // 沒填 → nil；其他 4 個方法「有」，但被呼叫會 nil panic
	msgs                   []model.Message
	err                    error

	// CreateUser 用
	createID    int64
	createErr   error
	gotUsername string // spy：handler 傳進來的參數
	gotHash     string

	// GetUser 用
	user   model.User
	getErr error
}

func (f *fakeDBStore) QueryMessage(ctx context.Context, limit int) ([]model.Message, error) {
	return f.msgs, f.err
}

func (f *fakeDBStore) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	f.gotUsername, f.gotHash = username, passwordHash
	return f.createID, f.createErr
}

func (f *fakeDBStore) GetUser(ctx context.Context, username string) (model.User, error) {
	return f.user, f.getErr
}

// handler test 用的假 limiter：照 test 設好的劇本回答，不真的計數
var _ store.RateLimiterInterface = (*fakeRateLimiter)(nil) // compile 時檢查 fake 有實作 interface

type fakeRateLimiter struct {
	blockKey   string        // 收到這個 key 就回 allowed=false
	retryAfter time.Duration // 被擋時回傳的等待時間
	err        error         // 不是 nil 就直接回傳這個錯（模擬 Redis 掛了）

	allowKeys  []string // spy：Allow 收到過的 key
	resetKeys  []string // spy：Reset 收到過的 key
	refundKeys []string // spy：Refund 收到過的 key
}

func (f *fakeRateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	f.allowKeys = append(f.allowKeys, key) // spy：記下被問過的 key
	if f.err != nil {
		return false, 0, f.err // 模擬 Redis 掛了；有 err 時呼叫端不看前兩個值
	}
	if key == f.blockKey {
		return false, f.retryAfter, nil
	}
	return true, 0, nil
}

func (f *fakeRateLimiter) Reset(ctx context.Context, key string) error {
	f.resetKeys = append(f.resetKeys, key) // spy：只記錄，不真的清什麼
	return nil
}

func (f *fakeRateLimiter) Refund(ctx context.Context, key string) error {
	f.refundKeys = append(f.refundKeys, key) // spy：只記錄，不真的退
	return nil
}

// handler test 共用的 token issuer：簽 / 驗都是純計算、沒有 I/O，直接用真的，不用 fake
var testTokens = func() *auth.TokenIssuer {
	issuer, err := auth.NewTokenIssuer(strings.Repeat("k", 32), time.Hour)
	if err != nil {
		panic(err)
	}
	return issuer
}()

// 一定簽失敗的 issuer：真的 HS256 不會失敗，只能用 fake 走到這條路
type failingTokenIssuer struct{}

func (failingTokenIssuer) IssueAccessToken(userID int64) (string, time.Duration, error) {
	return "", 0, errors.New("sign failed")
}
