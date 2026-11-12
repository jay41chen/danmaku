package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

type fakeLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error
	gotKey     string
	gotLimit   int
	gotWindow  time.Duration
}

func (f *fakeLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	f.gotKey, f.gotLimit, f.gotWindow = key, limit, window
	return f.allowed, f.retryAfter, f.err
}

func newBufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

var testRule = Rule{Name: "test:rule", Limit: 5, Window: time.Minute}

func TestRuleKey(t *testing.T) {
	if got := (Rule{Name: "login:username"}).Key("jay"); got != "rl:login:username:jay" {
		t.Errorf("Key = %q, want rl:login:username:jay", got)
	}
}

func TestCheckAllowed(t *testing.T) {
	l := &fakeLimiter{allowed: true}
	logger, buf := newBufLogger()

	ok, retry := Check(context.Background(), l, logger, testRule, "v")

	if !ok || retry != 0 {
		t.Errorf("got %v / %v, want true / 0", ok, retry)
	}
	if l.gotKey != "rl:test:rule:v" || l.gotLimit != 5 || l.gotWindow != time.Minute {
		t.Errorf("Allow got %q/%d/%v", l.gotKey, l.gotLimit, l.gotWindow)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log %q", buf.String())
	}
}

func TestCheckFailsOpenOnError(t *testing.T) {
	l := &fakeLimiter{err: errors.New("redis down")}
	logger, buf := newBufLogger()

	ok, retry := Check(context.Background(), l, logger, testRule, "v")

	if !ok || retry != 0 {
		t.Errorf("got %v / %v, want true / 0", ok, retry)
	}
	out := buf.String()
	for _, want := range []string{"rate limiter failed, fail open", "rule=test:rule", "key=rl:test:rule:v", "redis down"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestCheckBlockedCountsMetricAndLogsAttrs(t *testing.T) {
	rule := Rule{Name: "test:blocked", Limit: 1, Window: time.Second}
	l := &fakeLimiter{allowed: false, retryAfter: 3 * time.Second}
	logger, buf := newBufLogger()
	before := counterValue(t, rule.Name)

	ok, retry := Check(context.Background(), l, logger, rule, "v", "remote_addr", "1.2.3.4")

	if ok || retry != 3*time.Second {
		t.Errorf("got %v / %v, want false / 3s", ok, retry)
	}
	if got := counterValue(t, rule.Name); got != before+1 {
		t.Errorf("metric = %v, want %v", got, before+1)
	}
	out := buf.String()
	for _, want := range []string{"level=WARN", "rate limited", "rule=test:blocked", "key=rl:test:blocked:v", "remote_addr=1.2.3.4"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func counterValue(t *testing.T, rule string) float64 {
	t.Helper()
	var m dto.Metric
	if err := prom.RateLimitedTotal.WithLabelValues(rule).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
