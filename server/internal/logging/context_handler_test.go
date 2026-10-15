package logging

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/middleware"
)

func TestContextHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := ctxWithRequestID(t, "test-123")
	logger.InfoContext(ctx, "hello")

	result := buf.String()
	if !strings.Contains(result, `"request_id":"test-123"`) {
		t.Errorf("log missing request_id, got: %s", result)
	}
}

func TestContextHandlerWithoutRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "hello")

	result := buf.String()
	if strings.Contains(result, `"request_id"`) {
		t.Errorf("expected no request_id key, got: %s", result)
	}
}

// 確認 .With(...) 之後仍保留 ContextHandler（沒 override WithAttrs 的話 request_id 會消失）
func TestContextHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("component", "api")

	ctx := ctxWithRequestID(t, "test-123")
	logger.InfoContext(ctx, "hello")

	result := buf.String()
	if !strings.Contains(result, `"component":"api"`) {
		t.Errorf("log missing component, got: %s", result)
	}
	if !strings.Contains(result, `"request_id":"test-123"`) {
		t.Errorf("log missing request_id after With, got: %s", result)
	}
}

// 透過 RequestID middleware 產生帶 request_id 的 ctx，
// 讓 withRequestID 維持 unexported（只有 middleware 能設定 request_id）
func ctxWithRequestID(t *testing.T, id string) context.Context {
	t.Helper()
	var ctx context.Context
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(middleware.RequestIDHeader, id)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}
