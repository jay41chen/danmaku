package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/handler"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// 路由表的 test：走真的 registerRoutes，只把 handler 的依賴換成假的
// 每個 case 只驗「route 怎麼接」（method、要不要登入），不驗 handler 內部邏輯（那是 handler 自己的 test）

// 內嵌 nil interface：沒實作的方法被呼叫到會 nil panic
type healthyDB struct{ store.DBStoreInterface }

func (healthyDB) CheckHealth(ctx context.Context) error { return nil }

type healthyRedis struct{ store.RedisStoreInterface }

func (healthyRedis) CheckHealth(ctx context.Context) error { return nil }

// 一律驗證失敗的 parser：只用來確認「沒帶 / 帶了壞 token」會被擋
type rejectAllParser struct{}

func (rejectAllParser) ParseAccessToken(token string) (int64, error) {
	return 0, errors.New("invalid token")
}

func newTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerRoutes(mux, rejectAllParser{}, appHandlers{
		messages: handler.NewMessageHandler(nil, nil, nil),
		rooms:    handler.NewRoomHandler(nil),
		health:   handler.NewHealthHandler(healthyDB{}, healthyRedis{}),
		auth:     handler.NewAuthHandler(nil, nil, "", nil),
		ws:       handler.NewWebsocketHandler(nil, nil, nil),
	})
	return mux
}

func TestRoutes(t *testing.T) {
	mux := newTestMux()

	// method 不對由 mux 回 405（handler 沒有自己檢查了）
	t.Run("wrong method returns 405", func(t *testing.T) {
		cases := []struct{ method, path string }{
			{http.MethodGet, "/login"},
			{http.MethodGet, "/register"},
			{http.MethodGet, "/refresh"},
			{http.MethodGet, "/logout"},
			{http.MethodPost, "/healthcheck"},
			{http.MethodPost, "/ws"},
			{http.MethodPut, "/rooms/abc/messages"},
		}
		for _, tc := range cases {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
			}
		}
	})

	t.Run("healthcheck is public and reaches handler", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, "/healthcheck", nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("%s /healthcheck status = %d, want %d", method, rec.Code, http.StatusOK)
			}
		}
	})

	t.Run("metrics is public", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET /metrics status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	// /messages 已下架：公開的 tombstone，任何 method、沒帶 token 都是 410（不是 401）
	t.Run("/messages is gone for any method without token", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, "/messages", nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusGone {
				t.Errorf("%s /messages status = %d, want %d", method, rec.Code, http.StatusGone)
			}
			if !strings.Contains(rec.Body.String(), "/rooms/{public_id}/messages") {
				t.Errorf("%s /messages body = %q, want it to point to /rooms/{public_id}/messages", method, rec.Body.String())
			}
		}
	})
	// pattern 不帶結尾斜線 = 只比對完全相同的路徑
	t.Run("GET /messages/ returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/messages/", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /messages/ status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
	// 下面這些 401 case 自己 recover：RequireAuth 還沒實作時會 panic，
	// 不 recover 的話 panic 會中斷整個 test binary
	t.Run("POST /rooms/{id}/messages without token returns 401", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
			}
		}()
		req := httptest.NewRequest(http.MethodPost, "/rooms/abc/messages", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /rooms/abc/messages status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
	// POST /rooms 同樣要登入：handler 自己的 test 只能測「拿到 user id 之後怎麼處理」，
	// 擋不住有人把 r.post 改成 r.publicPost，只有這層 mux 的 test 能鎖住這個接法
	t.Run("POST /rooms without token returns 401", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
			}
		}()
		req := httptest.NewRequest(http.MethodPost, "/rooms", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /rooms status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
	t.Run("GET /me without token returns 401", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
			}
		}()
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /me status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}
