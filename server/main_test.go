package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
		health:   handler.NewHealthHandler(healthyDB{}, healthyRedis{}),
		auth:     handler.NewAuthHandler(nil, nil, "", nil),
		ws:       handler.NewWebsocketHandler(nil),
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
			{http.MethodPut, "/messages"},
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

	// 這個 case 放最後，而且自己 recover：RequireAuth 還沒實作時會 panic，
	// 不 recover 的話 panic 會中斷整個 test binary
	t.Run("POST /messages without token returns 401", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
			}
		}()
		req := httptest.NewRequest(http.MethodPost, "/messages", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /messages status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}
