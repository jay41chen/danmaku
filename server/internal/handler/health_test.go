package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// health check 用的假依賴：err 不是 nil 就回它；panics 為 true 就直接 panic（模擬 client 內部的 bug）
type fakeHealthChecker struct {
	err    error
	panics bool
}

func (f fakeHealthChecker) CheckHealth(ctx context.Context) error {
	if f.panics {
		panic("injected by test: CheckHealth")
	}
	return f.err
}

func serveHealth(t *testing.T, db, redis healthChecker) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHealthHandler(db, redis).HandleHealthCheck(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestHealthCheckOK(t *testing.T) {
	code, body := serveHealth(t, fakeHealthChecker{}, fakeHealthChecker{})

	if code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("got %d %v, want 200 status=ok", code, body)
	}
}

func TestHealthCheckDBError(t *testing.T) {
	code, body := serveHealth(t, fakeHealthChecker{err: errors.New("db down")}, fakeHealthChecker{})

	if code != http.StatusInternalServerError || body["db"] != "error" || body["redis"] != "" {
		t.Errorf("got %d %v, want 500 with only db=error", code, body)
	}
}

// CheckHealth 跑在 handler 自己開的 goroutine 上，RecoverPanic 接不到：panic 要轉成 error（回 500），不能讓 process 掛掉
// 沒有轉的話，這個 test 會讓整個 test binary crash
func TestHealthCheckPanicBecomesError(t *testing.T) {
	code, body := serveHealth(t, fakeHealthChecker{}, fakeHealthChecker{panics: true})

	if code != http.StatusInternalServerError || body["redis"] != "error" || body["db"] != "" {
		t.Errorf("got %d %v, want 500 with only redis=error", code, body)
	}
}
