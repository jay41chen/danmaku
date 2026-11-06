package loadtiming

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 多個 goroutine 讀寫的 buffer（logger 在 Run 的 goroutine 寫、test 在主 goroutine 讀）
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHistogramObserveAndDrain(t *testing.T) {
	h := NewHistogram()
	h.Observe(400*time.Microsecond, false) // <= 0.5ms
	h.Observe(3*time.Millisecond, false)   // <= 3ms
	h.Observe(3*time.Millisecond, true)
	h.Observe(10*time.Second, false) // overflow

	snap := h.Drain()

	if snap.Count != 4 || snap.Errors != 1 {
		t.Fatalf("count/errors = %d/%d, want 4/1", snap.Count, snap.Errors)
	}
	idx := func(ms float64) int {
		for i, b := range BoundsMs {
			if b == ms {
				return i
			}
		}
		t.Fatalf("no bound %v", ms)
		return -1
	}
	if snap.Buckets[idx(0.5)] != 1 || snap.Buckets[idx(3)] != 2 || snap.Buckets[len(BoundsMs)] != 1 {
		t.Errorf("buckets = %v", snap.Buckets)
	}
	// Drain 之後歸零：下一段從 0 開始
	if again := h.Drain(); again.Count != 0 || again.Errors != 0 || again.SumMs != 0 {
		t.Errorf("second Drain = %+v, want zeros", again)
	}
}

func TestQuantile(t *testing.T) {
	// 100 筆：90 筆在 1ms、9 筆在 10ms、1 筆在 100ms
	b := make([]int64, len(BoundsMs)+1)
	at := func(ms float64) int {
		for i, bound := range BoundsMs {
			if bound == ms {
				return i
			}
		}
		panic("bound")
	}
	b[at(1)], b[at(10)], b[at(100)] = 90, 9, 1

	cases := []struct {
		q    float64
		want float64
	}{{0.50, 1}, {0.90, 1}, {0.91, 10}, {0.99, 10}, {1.0, 100}}
	for _, tc := range cases {
		if got := Quantile(b, tc.q); got != tc.want {
			t.Errorf("Quantile(%v) = %v, want %v", tc.q, got, tc.want)
		}
	}
	// 小樣本的邊界：10 筆，9 筆 1ms、1 筆 10ms。p90 = 第 9 筆 = 1ms；p95 = 第 ceil(9.5)=10 筆 = 10ms（不能被「平均」掉）
	small := make([]int64, len(BoundsMs)+1)
	small[at(1)], small[at(10)] = 9, 1
	if got := Quantile(small, 0.90); got != 1 {
		t.Errorf("small p90 = %v, want 1", got)
	}
	if got := Quantile(small, 0.95); got != 10 {
		t.Errorf("small p95 = %v, want 10", got)
	}
	if got := Quantile(make([]int64, len(BoundsMs)+1), 0.5); got != 0 {
		t.Errorf("empty Quantile = %v, want 0", got)
	}
	over := make([]int64, len(BoundsMs)+1)
	over[len(BoundsMs)] = 1
	if got := Quantile(over, 0.5); got != -1 {
		t.Errorf("overflow Quantile = %v, want -1", got)
	}
}

func TestBuildLineUsesDeltas(t *testing.T) {
	prev := PoolSample{AcquireCount: 100, AcquireDuration: 50 * time.Millisecond, EmptyAcquireCount: 3, CanceledAcquireCount: 1}
	cur := PoolSample{AcquireCount: 160, AcquireDuration: 80 * time.Millisecond, EmptyAcquireCount: 10, CanceledAcquireCount: 1, Acquired: 4, Idle: 6, Max: 10}
	line := BuildLine(prev, cur, HistogramSnapshot{Buckets: make([]int64, len(BoundsMs)+1)}, 5*time.Second)

	p := line.Pool
	if p.AcquireCount != 60 || p.AcquireDurationMs != 30 || p.EmptyAcquireCount != 7 || p.CanceledAcquireCount != 0 {
		t.Errorf("pool deltas = %+v", p)
	}
	if p.Acquired != 4 || p.Idle != 6 || p.Max != 10 || line.IntervalMs != 5000 {
		t.Errorf("pool gauges / interval = %+v / %d", p, line.IntervalMs)
	}
}

// Run 實際 log 出一行，而且 msg / data 的結構是壓測工具依賴的格式
func TestRunLogsParsableLine(t *testing.T) {
	buf := &syncBuf{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	hist, limiter, publish := NewHistogram(), NewHistogram(), NewHistogram()
	hist.Observe(2*time.Millisecond, false)
	limiter.Observe(3*time.Millisecond, false)
	limiter.Observe(3*time.Millisecond, true)
	publish.Observe(7*time.Millisecond, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var n atomic.Int64
	go func() {
		Run(ctx, logger, 20*time.Millisecond, Hists{Create: hist, Limiter: limiter, Publish: publish}, func() PoolSample { return PoolSample{AcquireCount: n.Add(1) * 10, Max: 10} })
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for !strings.Contains(buf.String(), "\n") {
		select {
		case <-deadline:
			t.Fatal("no line logged")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	first, _, _ := strings.Cut(buf.String(), "\n")
	var rec struct {
		Msg  string `json:"msg"`
		Data Line   `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &rec); err != nil {
		t.Fatalf("unmarshal %q: %v", first, err)
	}
	if rec.Msg != LogMessage || rec.Data.Create.Count != 1 || rec.Data.Create.P50Ms != 2 || rec.Data.Pool.AcquireCount != 10 || rec.Data.Pool.Max != 10 {
		t.Errorf("record = %+v", rec)
	}
	// limiter / publish 也有各自的分布，不會混進 CreateMessage
	if l := rec.Data.Limiter; l.Count != 2 || l.Errors != 1 || l.P50Ms != 3 {
		t.Errorf("limiter = %+v, want count 2 / errors 1 / p50 3", l)
	}
	if p := rec.Data.Publish; p.Count != 1 || p.P50Ms != 7.5 || p.SumMs < 6.9 || p.SumMs > 7.1 {
		t.Errorf("publish = %+v, want count 1 / p50 7.5 (bucket bound) / sum 7ms", p)
	}
}
