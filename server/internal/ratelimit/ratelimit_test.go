package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/prometheus/promtest"
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

// Allow 一直擋到 ctx 結束才回傳（模擬 Redis 不回應），並記下收到的 ctx 期限
type blockingLimiter struct {
	deadline    time.Time
	hasDeadline bool
}

func (b *blockingLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	b.deadline, b.hasDeadline = ctx.Deadline()
	<-ctx.Done()
	return false, 0, ctx.Err()
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

	ok, retry, err := Check(context.Background(), l, logger, testRule, "v")

	if !ok || retry != 0 || err != nil {
		t.Errorf("got %v / %v / %v, want true / 0 / nil", ok, retry, err)
	}
	if l.gotKey != "rl:test:rule:v" || l.gotLimit != 5 || l.gotWindow != time.Minute {
		t.Errorf("Allow got %q/%d/%v", l.gotKey, l.gotLimit, l.gotWindow)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log %q", buf.String())
	}
}

func TestCheckFailsOpenOnError(t *testing.T) {
	rule := Rule{Name: "test:open", Limit: 5, Window: time.Minute}
	l := &fakeLimiter{err: errors.New("redis down")}
	logger, buf := newBufLogger()
	before := promtest.RateLimiterErrorCount(rule.Name)

	ok, retry, err := Check(context.Background(), l, logger, rule, "v")

	if !ok || retry != 0 || err != nil {
		t.Errorf("got %v / %v / %v, want true / 0 / nil", ok, retry, err)
	}
	if got := promtest.RateLimiterErrorCount(rule.Name); got != before+1 {
		t.Errorf("error metric = %v, want %v", got, before+1)
	}
	out := buf.String()
	for _, want := range []string{"rate limiter failed, fail open", "rule=test:open", "key=rl:test:open:v", "redis down"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestCheckBlockedCountsMetricAndLogsAttrs(t *testing.T) {
	rule := Rule{Name: "test:blocked", Limit: 1, Window: time.Second}
	l := &fakeLimiter{allowed: false, retryAfter: 3 * time.Second}
	logger, buf := newBufLogger()
	before := promtest.RateLimitedCount(rule.Name)

	ok, retry, err := Check(context.Background(), l, logger, rule, "v", "remote_addr", "1.2.3.4")

	if ok || retry != 3*time.Second || err != nil {
		t.Errorf("got %v / %v / %v, want false / 3s / nil", ok, retry, err)
	}
	if got := promtest.RateLimitedCount(rule.Name); got != before+1 {
		t.Errorf("metric = %v, want %v", got, before+1)
	}
	out := buf.String()
	for _, want := range []string{"level=WARN", "rate limited", "rule=test:blocked", "key=rl:test:blocked:v", "remote_addr=1.2.3.4"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

// Redis 不回應時，Check 要在 checkTimeout 內放棄並 fail open，不能卡住 request
func TestCheckFailsOpenOnTimeout(t *testing.T) {
	l := &blockingLimiter{}
	logger, buf := newBufLogger()

	start := time.Now()
	ok, retry, err := Check(context.Background(), l, logger, testRule, "v")
	elapsed := time.Since(start)

	if !ok || retry != 0 || err != nil {
		t.Errorf("got %v / %v / %v, want true / 0 / nil", ok, retry, err)
	}
	if elapsed < checkTimeout-20*time.Millisecond || elapsed > time.Second {
		t.Errorf("Check took %v, want about %v (< 1s)", elapsed, checkTimeout)
	}
	if !l.hasDeadline {
		t.Fatal("ctx passed to Allow has no deadline")
	}
	if remain := l.deadline.Sub(start); remain > checkTimeout+50*time.Millisecond {
		t.Errorf("deadline = %v after start, want <= ~%v", remain, checkTimeout)
	}
	out := buf.String()
	for _, want := range []string{"rate limiter failed, fail open", "rule=test:rule", "context deadline exceeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

// FailClosed 規則：limiter 出錯時擋下並回 ErrRateLimiterUnavailable
func TestCheckFailsClosedOnError(t *testing.T) {
	rule := Rule{Name: "test:closed", Limit: 5, Window: time.Minute, FailClosed: true}
	l := &fakeLimiter{err: errors.New("redis down")}
	logger, buf := newBufLogger()
	before := promtest.RateLimiterErrorCount(rule.Name)

	ok, retry, err := Check(context.Background(), l, logger, rule, "v")

	if ok || retry != 0 || !errors.Is(err, model.ErrRateLimiterUnavailable) {
		t.Errorf("got %v / %v / %v, want false / 0 / ErrRateLimiterUnavailable", ok, retry, err)
	}
	if got := promtest.RateLimiterErrorCount(rule.Name); got != before+1 {
		t.Errorf("error metric = %v, want %v", got, before+1)
	}
	out := buf.String()
	for _, want := range []string{"rate limiter failed, fail closed", "rule=test:closed", "key=rl:test:closed:v", "redis down"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

// FailClosed 規則 + Redis 不回應：一樣在 checkTimeout 內放棄，並回 ErrRateLimiterUnavailable
func TestCheckFailsClosedOnTimeout(t *testing.T) {
	rule := Rule{Name: "test:closed:timeout", Limit: 5, Window: time.Minute, FailClosed: true}
	l := &blockingLimiter{}
	logger, buf := newBufLogger()
	before := promtest.RateLimiterErrorCount(rule.Name)

	start := time.Now()
	ok, retry, err := Check(context.Background(), l, logger, rule, "v")
	elapsed := time.Since(start)

	if ok || retry != 0 || !errors.Is(err, model.ErrRateLimiterUnavailable) {
		t.Errorf("got %v / %v / %v, want false / 0 / ErrRateLimiterUnavailable", ok, retry, err)
	}
	if elapsed < checkTimeout-20*time.Millisecond || elapsed > time.Second {
		t.Errorf("Check took %v, want about %v (< 1s)", elapsed, checkTimeout)
	}
	if got := promtest.RateLimiterErrorCount(rule.Name); got != before+1 {
		t.Errorf("error metric = %v, want %v", got, before+1)
	}
	if out := buf.String(); !strings.Contains(out, "rate limiter failed, fail closed") {
		t.Errorf("log %q missing fail closed message", out)
	}
}
