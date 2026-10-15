package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	handler := Logging(logger)(mock)

	req := httptest.NewRequest(http.MethodPost, "/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	result := buf.String()
	if !strings.Contains(result, `"status":405`) {
		t.Errorf("log missing status 405, got: %s", result)
	}
	if !strings.Contains(result, `"level":"WARN"`) {
		t.Errorf("log missing level WARN, got: %s", result)
	}
	if !strings.Contains(result, `"component":"http"`) {
		t.Errorf("log missing component, got: %s", result)
	}
}

func TestIgnoreMetrics(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	called := false
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := Logging(logger)(mock)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Errorf("next handler was not called for /metrics")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log for /metrics, got: %s", buf.String())
	}
}

func TestLogLevel(t *testing.T) {
	codes := map[int]slog.Level{
		200: slog.LevelInfo,
		101: slog.LevelInfo,
		404: slog.LevelWarn,
		499: slog.LevelWarn,
		500: slog.LevelError,
		503: slog.LevelError,
	}
	for code, expected := range codes {
		if logLevel := determineLogLevel(code); logLevel != expected {
			t.Errorf("determineLogLevel(%d) = %v, expected %v", code, logLevel, expected)
		}
	}
}
