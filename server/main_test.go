package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/handler"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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
	t.Run("GET /me/rooms without token returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/me/rooms", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /me/rooms status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
	// 這個 mux 的 RoomHandler 沒有 DB：帶壞的 limit 讓 handler 在碰 DB 前就回 400，
	// 400 代表請求通過了 auth、進到 handler（公開）
	t.Run("GET /rooms without token is public", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rooms?limit=abc", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET /rooms?limit=abc status = %d, want %d", rec.Code, http.StatusBadRequest)
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

// 讀出某個 metric 目前的 dto 值（testutil 會多拉依賴，這裡直接 Write）
func writeMetric(t *testing.T, c any) *dto.Metric {
	t.Helper()
	m, ok := c.(prometheus.Metric)
	if !ok {
		t.Fatalf("%T is not a prometheus.Metric", c)
	}
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return &out
}

func counterValue(t *testing.T, method, path, status string) float64 {
	t.Helper()
	return writeMetric(t, prom.HttpRequestCount.WithLabelValues(method, path, status)).GetCounter().GetValue()
}

// histogram 的 sample 總數
func histogramCount(t *testing.T, method, path string) uint64 {
	t.Helper()
	return writeMetric(t, prom.HttpRequestDuration.WithLabelValues(method, path)).GetHistogram().GetSampleCount()
}

// 守「接線」：走 serviceHandler 的真 chain，確認 HttpMetrics 的 path / method label 真的用了轉換後的值
// internal/prometheus 的 table test 只測得到 getRoutePath / getMethod 本身，抓不到
// 有人在 HttpMetrics 和 mux 之間插了會複製 request 的 middleware（會讓全部變 unmatched）
// metric 是全域的、跨 test 累加，所以只比 before/after 的差值
func TestServiceHandlerMetricsUseRoutePattern(t *testing.T) {
	h := serviceHandler(newTestMux(), nil)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantLabel  string
	}{
		{"matched route uses pattern", "/healthcheck", http.StatusOK, "/healthcheck"},
		{"unknown route is unmatched", "/no-such-route-xyz", http.StatusNotFound, "unmatched"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := strconv.Itoa(tt.wantStatus)
			countBefore := counterValue(t, http.MethodGet, tt.wantLabel, status)
			histBefore := histogramCount(t, http.MethodGet, tt.wantLabel)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", tt.path, rec.Code, tt.wantStatus)
			}
			if got := counterValue(t, http.MethodGet, tt.wantLabel, status) - countBefore; got != 1 {
				t.Errorf("request count delta for {GET, %s, %s} = %v, want 1", tt.wantLabel, status, got)
			}
			if got := histogramCount(t, http.MethodGet, tt.wantLabel) - histBefore; got != 1 {
				t.Errorf("duration sample count delta for {GET, %s} = %d, want 1", tt.wantLabel, got)
			}
		})
	}

	// 這個 case 的 URL path（/rooms/abc/messages）≠ pattern path（/rooms/{public_id}/messages）
	// 能分辨 label 用的是 pattern 還是 r.URL.Path（healthcheck 的 URL path 跟 pattern 一樣，分不出來）
	// 沒帶 Authorization，RequireAuth 在進 handler 前就擋掉，不會碰到 nil 的依賴
	t.Run("URL path differs from pattern uses pattern", func(t *testing.T) {
		const label = "/rooms/{public_id}/messages"
		countBefore := counterValue(t, http.MethodPost, label, "401")
		histBefore := histogramCount(t, http.MethodPost, label)

		req := httptest.NewRequest(http.MethodPost, "/rooms/abc/messages", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST /rooms/abc/messages status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := counterValue(t, http.MethodPost, label, "401") - countBefore; got != 1 {
			t.Errorf("request count delta for {POST, %s, 401} = %v, want 1", label, got)
		}
		if got := histogramCount(t, http.MethodPost, label) - histBefore; got != 1 {
			t.Errorf("duration sample count delta for {POST, %s} = %d, want 1", label, got)
		}
	})

	// 非標準 method 歸成 "other"，不能讓 client 決定 label 的值；mux 對 /healthcheck 回 405，所以 path 是 unmatched
	t.Run("non-standard method is labeled other", func(t *testing.T) {
		countBefore := counterValue(t, "other", "unmatched", "405")
		histBefore := histogramCount(t, "other", "unmatched")

		req := httptest.NewRequest("FOOBAR123", "/healthcheck", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("FOOBAR123 /healthcheck status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
		if got := counterValue(t, "other", "unmatched", "405") - countBefore; got != 1 {
			t.Errorf("request count delta for {other, unmatched, 405} = %v, want 1", got)
		}
		if got := histogramCount(t, "other", "unmatched") - histBefore; got != 1 {
			t.Errorf("duration sample count delta for {other, unmatched} = %d, want 1", got)
		}
	})
}
