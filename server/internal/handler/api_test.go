package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

type fakeDBStore struct {
	store.DBStoreInterface // 沒填 → nil；其他 4 個方法「有」，但被呼叫會 nil panic
	msgs                   []model.Message
	err                    error

	// CreateUser 用
	createID    int64
	createErr   error
	gotUsername string // spy：handler 傳進來的參數
	gotHash     string
}

func (f *fakeDBStore) QueryMessage(ctx context.Context, limit int) ([]model.Message, error) {
	return f.msgs, f.err
}

func (f *fakeDBStore) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	f.gotUsername, f.gotHash = username, passwordHash
	return f.createID, f.createErr
}

func TestHandleMessages(t *testing.T) {
	t.Run("db error returns 500", func(t *testing.T) {
		h := NewAPIHandler(&fakeDBStore{err: errors.New("db down")}, nil) // redis 用不到，傳 nil
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleMessages(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
	t.Run("returns oldest first", func(t *testing.T) {
		input := []model.Message{{Content: "c"},
			{Content: "b"},
			{Content: "a"}}
		// copy一份input並翻轉，這是我們期待的結果
		expect := slices.Clone(input)
		slices.Reverse(expect)
		h := NewAPIHandler(&fakeDBStore{msgs: input},
			nil) // redis 用不到，傳 nil
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleMessages(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var got []model.Message
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}

		if !slices.Equal(expect, got) {
			t.Errorf("expect = %q, got %q", expect, got)
		}
	})
}
