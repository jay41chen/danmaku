package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/usecase"
)

// 守住 auth 被限流時 log 要帶 remote_addr，才查得到攻擊來源 IP
func TestCheckRateLogsRemoteAddrWhenBlocked(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "1.2.3.4:5"
	limiter := &fakeRateLimiter{blockKey: usecase.LoginIPRule.Key("v"), retryAfter: time.Second}

	allowed, _ := checkRate(req, limiter, logger, usecase.LoginIPRule, "v")

	if allowed {
		t.Fatal("allowed = true, want false")
	}
	if !strings.Contains(buf.String(), "remote_addr=1.2.3.4:5") {
		t.Errorf("log %q does not contain remote_addr=1.2.3.4:5", buf.String())
	}
}
