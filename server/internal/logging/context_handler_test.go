package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/slogtest"

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

// 解析 JSON 輸出的 helper
func decodeLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("invalid json %q: %v", buf.String(), err)
	}
	return m
}

// WithGroup 之後 request_id 仍要在最上層，不能被包進 group
func TestContextHandlerWithGroupKeepsRequestIDTopLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).WithGroup("db")

	logger.InfoContext(ctxWithRequestID(t, "abc"), "q", "sql", "select 1")

	m := decodeLog(t, &buf)
	if m["request_id"] != "abc" {
		t.Errorf("request_id = %v, want top-level abc; log: %s", m["request_id"], buf.String())
	}
	db, _ := m["db"].(map[string]any)
	if db["sql"] != "select 1" {
		t.Errorf("db.sql = %v, want select 1; log: %s", db["sql"], buf.String())
	}
	if _, nested := db["request_id"]; nested {
		t.Errorf("request_id nested in group: %s", buf.String())
	}
}

func TestContextHandlerNestedGroups(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).
		With("component", "api").WithGroup("a").With("x", 1).WithGroup("b")

	logger.InfoContext(ctxWithRequestID(t, "abc"), "m", "y", 2)

	got := decodeLog(t, &buf)
	delete(got, "time")
	delete(got, "level")
	delete(got, "msg")
	want := map[string]any{
		"component":  "api",
		"a":          map[string]any{"x": float64(1), "b": map[string]any{"y": float64(2)}},
		"request_id": "abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("log = %v, want %v", got, want)
	}
}

func TestContextHandlerEmptyGroupOmitted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).WithGroup("g")

	logger.InfoContext(context.Background(), "m")

	if _, ok := decodeLog(t, &buf)["g"]; ok {
		t.Errorf("empty group g should be omitted: %s", buf.String())
	}
}

// WithAttrs / WithGroup 不能改到 receiver 共用的 slice：從同一個 parent 分出去的兩個 handler 互不影響
func TestContextHandlerWithGroupDoesNotMutateParent(t *testing.T) {
	var buf bytes.Buffer
	parent := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).WithGroup("a")
	c1 := parent.With("k", 1)
	c2 := parent.With("k", 2)

	c1.Info("m")
	c2.Info("m")

	dec := json.NewDecoder(&buf)
	for _, want := range []float64{1, 2} {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if a, _ := m["a"].(map[string]any); a["k"] != want {
			t.Errorf("a.k = %v, want %v", a["k"], want)
		}
	}
}

func TestContextHandlerSlogtest(t *testing.T) {
	var buf bytes.Buffer
	h := NewContextHandler(slog.NewJSONHandler(&buf, nil))

	results := func() []map[string]any {
		var ms []map[string]any
		for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, m)
		}
		return ms
	}
	if err := slogtest.TestHandler(h, results); err != nil {
		t.Error(err)
	}
}
