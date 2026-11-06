package httpx

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

const internalErrorBody = "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal server error\"}}\n"

func TestClientIP(t *testing.T) {
	cases := []struct {
		name             string
		trustedHeaderKey string
		headers          map[string]string
		remoteAddr       string
		want             string
	}{
		// 沒設定要信任的 header：client 偽造的值不能被採用
		{"no header configured", "", map[string]string{"X-Real-IP": "6.6.6.6"}, "10.0.0.1:5555", "10.0.0.1"},
		{"header configured", "X-Real-IP", map[string]string{"X-Real-IP": "1.2.3.4"}, "10.0.0.1:5555", "1.2.3.4"},
		// 只能讀設定的那個 header，不能寫死 X-Real-IP：別的 header 再像 IP 都不採用
		{"other platform header", "CF-Connecting-IP", map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Real-IP": "6.6.6.6"}, "10.0.0.1:5555", "1.2.3.4"},
		{"header missing", "X-Real-IP", nil, "10.0.0.1:5555", "10.0.0.1"},
		{"header not an IP", "X-Real-IP", map[string]string{"X-Real-IP": "garbage"}, "10.0.0.1:5555", "10.0.0.1"},
		// 同一個 IP 不同寫法要變成同一個 key（IPv4-mapped IPv6 → IPv4）
		{"header normalized", "X-Real-IP", map[string]string{"X-Real-IP": "::ffff:1.2.3.4"}, "10.0.0.1:5555", "1.2.3.4"},
		// IPv6 的 RemoteAddr 有中括號，要靠 SplitHostPort 去掉
		{"IPv6 remote", "", nil, "[::1]:5555", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			got := ClientIP(req, tc.trustedHeaderKey)

			if got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/x", nil)
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		WriteJSON(rec, r, logger, http.StatusCreated, map[string]int{"id": 1})

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want 201", rec.Code)
		}
		// 用 Result()：header 如果是在 WriteHeader 之後才設，這裡會讀不到
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if want := "{\"id\":1}\n"; rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}
	})

	t.Run("marshal failure", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/x", nil)
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		WriteJSON(rec, r, logger, http.StatusOK, make(chan int))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if want := internalErrorBody; rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}
		if !strings.Contains(buf.String(), `"level":"ERROR"`) || !strings.Contains(buf.String(), `"msg":"marshal json response failed"`) {
			t.Errorf("log = %s, want ERROR entry 'marshal json response failed'", buf.String())
		}
	})
}

func TestWriteJSONMarshalFailureOverwritesCacheControl(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "public, s-maxage=1")
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	WriteJSON(rec, httptest.NewRequest("GET", "/x", nil), logger, http.StatusOK, make(chan int))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := rec.Result().Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on the 500", got)
	}
}

func TestWriteErrorHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "public, s-maxage=1")
	rec.Header().Set("WWW-Authenticate", "Bearer")
	rec.Header().Set("Retry-After", "30")

	WriteError(rec, http.StatusTooManyRequests, "rate_limited", "too many requests")

	h := rec.Result().Header
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (overwrites earlier public, s-maxage=1)", got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer kept", got)
	}
	if got := h.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 kept", got)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteError(rec, http.StatusConflict, "username_taken", "username already taken")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	// 用 Result()：header 如果是在 WriteHeader 之後才設，這裡會讀不到
	if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// 逐字比對：欄位名稱、順序、結尾換行都是 API 合約
	want := "{\"error\":{\"code\":\"username_taken\",\"message\":\"username already taken\"}}\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestWriteCodedError(t *testing.T) {
	coded := &model.CodedError{Code: "content_empty", Message: "content must not be empty"}
	const codedBody = "{\"error\":{\"code\":\"content_empty\",\"message\":\"content must not be empty\"}}\n"

	t.Run("coded error uses its own code and message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		WriteCodedError(rec, httptest.NewRequest("POST", "/x", nil), logger, http.StatusBadRequest, coded)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if rec.Body.String() != codedBody {
			t.Errorf("body = %q, want %q", rec.Body.String(), codedBody)
		}
		if buf.Len() != 0 {
			t.Errorf("unexpected log for a coded error: %s", buf.String())
		}
	})

	t.Run("wrapped coded error is found with errors.As", func(t *testing.T) {
		rec := httptest.NewRecorder()
		logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
		wrapped := fmt.Errorf("validate: %w", coded)

		WriteCodedError(rec, httptest.NewRequest("POST", "/x", nil), logger, http.StatusBadRequest, wrapped)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		// message 是 CodedError 的，不是外層 "validate: ..." 的文字
		if rec.Body.String() != codedBody {
			t.Errorf("body = %q, want %q", rec.Body.String(), codedBody)
		}
	})

	t.Run("plain error never reaches the client", func(t *testing.T) {
		const secret = "password=hunter2 dsn=postgres://u:s3cr3t@db"
		rec := httptest.NewRecorder()
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))

		// 呼叫端傳 400，但沒有 code 的 error 一律是 500
		WriteCodedError(rec, httptest.NewRequest("POST", "/x", nil), logger, http.StatusBadRequest, errors.New(secret))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if rec.Body.String() != internalErrorBody {
			t.Errorf("body = %q, want %q", rec.Body.String(), internalErrorBody)
		}
		if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "s3cr3t") {
			t.Errorf("body leaks the error text: %q", rec.Body.String())
		}
		// 真正原因要進 log（Error 等級），不然沒人查得到
		if !strings.Contains(buf.String(), `"level":"ERROR"`) || !strings.Contains(buf.String(), "hunter2") {
			t.Errorf("log = %s, want ERROR entry containing the real error", buf.String())
		}
	})
}
