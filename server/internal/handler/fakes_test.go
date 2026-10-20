package handler

import (
	"context"

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
