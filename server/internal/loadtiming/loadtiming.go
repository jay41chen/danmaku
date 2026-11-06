// Package loadtiming 是壓測專用的 DB 計時：只有 LOADTEST_TIMING_LOG 開啟時才會被 main 接上。
// 每隔一段時間（預設 5 秒）log 一行 JSON：pgxpool 的 Stat 差值，加上這段期間 CreateMessage 的耗時分布（固定 bucket），
// 讓壓測工具（cmd/loadtest）事後從 log 檔算 p50 / p99。不進 /metrics、不開 route；關閉時 main 不會建立這裡的任何東西。
package loadtiming

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// log 那一行的 msg；壓測工具靠它從一般 log 裡挑出這種行
const LogMessage = "loadtest_timing"

// 預設的輸出間隔
const DefaultInterval = 5 * time.Second

// 耗時 bucket 的上界（毫秒，含上界）；超過最後一個的算進 overflow bucket
// 每一行都會帶這份邊界，讀的人不用跟 code 同步
var BoundsMs = []float64{0.25, 0.5, 1, 1.5, 2, 3, 4, 5, 7.5, 10, 15, 20, 30, 50, 75, 100, 150, 200, 300, 500, 750, 1000, 2000, 5000}

// 耗時分布：Observe 只做一次 atomic 加法（不拿鎖），Drain 取走並歸零
type Histogram struct {
	// 長度 len(BoundsMs)+1，最後一格是 overflow
	counts []atomic.Int64
	errors atomic.Int64
	sumNs  atomic.Int64
}

func NewHistogram() *Histogram {
	return &Histogram{counts: make([]atomic.Int64, len(BoundsMs)+1)}
}

// 記一次耗時；failed = 這次呼叫回了 error（仍然算進分布，另外計數）
func (h *Histogram) Observe(d time.Duration, failed bool) {
	ms := float64(d) / float64(time.Millisecond)
	i := len(BoundsMs) // overflow
	for j, bound := range BoundsMs {
		if ms <= bound {
			i = j
			break
		}
	}
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
	if failed {
		h.errors.Add(1)
	}
}

// Drain 之後的分布快照
type HistogramSnapshot struct {
	Buckets []int64
	Count   int64
	Errors  int64
	SumMs   float64
}

// 取走自上次 Drain 以來的資料並歸零
func (h *Histogram) Drain() HistogramSnapshot {
	snap := HistogramSnapshot{Buckets: make([]int64, len(h.counts))}
	for i := range h.counts {
		n := h.counts[i].Swap(0)
		snap.Buckets[i] = n
		snap.Count += n
	}
	snap.Errors = h.errors.Swap(0)
	snap.SumMs = float64(h.sumNs.Swap(0)) / float64(time.Millisecond)
	return snap
}

// Quantile 回傳第 q 分位（0 < q <= 1）落在的 bucket 的上界（毫秒），所以是上界估計、不是精確值
// 沒有資料回 0；落在 overflow bucket 回 -1（比最大的邊界還慢）
func Quantile(buckets []int64, q float64) float64 {
	var total int64
	for _, n := range buckets {
		total += n
	}
	if total == 0 {
		return 0
	}
	// nearest-rank：第 ceil(q*total) 筆
	rank := int64(q * float64(total))
	if float64(rank) < q*float64(total) {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for i, n := range buckets {
		seen += n
		if seen >= rank {
			if i >= len(BoundsMs) {
				return -1
			}
			return BoundsMs[i]
		}
	}
	return -1
}

// pgxpool.Stat 的累計值與當下值：由 main 從 pool 轉過來，這個 package 不依賴 pgx
type PoolSample struct {
	AcquireCount         int64
	AcquireDuration      time.Duration
	EmptyAcquireCount    int64
	CanceledAcquireCount int64
	Acquired             int32
	Idle                 int32
	Max                  int32
}

// log 裡的 pool 欄位：前四個是這段期間的差值，後三個是當下值
type PoolLine struct {
	AcquireCount         int64   `json:"acquire_count"`
	AcquireDurationMs    float64 `json:"acquire_duration_ms"`
	EmptyAcquireCount    int64   `json:"empty_acquire_count"`
	CanceledAcquireCount int64   `json:"canceled_acquire_count"`
	Acquired             int32   `json:"acquired"`
	Idle                 int32   `json:"idle"`
	Max                  int32   `json:"max"`
}

// log 裡一個耗時分布的欄位：CreateMessage、limiter Allow、Redis PUBLISH 共用同一種格式
type CreateLine struct {
	Count    int64     `json:"count"`
	Errors   int64     `json:"errors"`
	SumMs    float64   `json:"sum_ms"`
	P50Ms    float64   `json:"p50_ms"` // bucket 上界；-1 = 超過最大邊界
	P99Ms    float64   `json:"p99_ms"`
	BoundsMs []float64 `json:"bounds_ms"`
	Buckets  []int64   `json:"buckets"` // 長度 len(BoundsMs)+1，最後一格是 overflow
}

// 一行 log 的內容（放在 JSON 的 "data" 欄位下）
type Line struct {
	IntervalMs int64      `json:"interval_ms"`
	Pool       PoolLine   `json:"pool"`
	Create     CreateLine `json:"create_message"`
	Limiter    CreateLine `json:"limiter_allow"` // 限流 Allow 呼叫（一次 POST 會問兩條規則，所以 count 約為 POST 的兩倍）
	Publish    CreateLine `json:"redis_publish"` // 廣播用的 Redis PUBLISH
}

// 三個耗時分布：Create = CreateMessage、Limiter = 限流 Allow、Publish = Redis PUBLISH。nil 的那個當作沒有資料
type Hists struct {
	Create, Limiter, Publish *Histogram
}

func (h *Histogram) drainOrEmpty() HistogramSnapshot {
	if h == nil {
		return HistogramSnapshot{Buckets: make([]int64, len(BoundsMs)+1)}
	}
	return h.Drain()
}

// 組出一行：純計算，方便測試
func BuildLine(prev, cur PoolSample, snap HistogramSnapshot, interval time.Duration) Line {
	return Line{
		IntervalMs: interval.Milliseconds(),
		Pool: PoolLine{
			AcquireCount:         cur.AcquireCount - prev.AcquireCount,
			AcquireDurationMs:    float64(cur.AcquireDuration-prev.AcquireDuration) / float64(time.Millisecond),
			EmptyAcquireCount:    cur.EmptyAcquireCount - prev.EmptyAcquireCount,
			CanceledAcquireCount: cur.CanceledAcquireCount - prev.CanceledAcquireCount,
			Acquired:             cur.Acquired,
			Idle:                 cur.Idle,
			Max:                  cur.Max,
		},
		Create: HistLine(snap),
	}
}

// 分布快照 → log 裡的欄位
func HistLine(snap HistogramSnapshot) CreateLine {
	return CreateLine{
		Count:    snap.Count,
		Errors:   snap.Errors,
		SumMs:    snap.SumMs,
		P50Ms:    Quantile(snap.Buckets, 0.50),
		P99Ms:    Quantile(snap.Buckets, 0.99),
		BoundsMs: BoundsMs,
		Buckets:  snap.Buckets,
	}
}

// 每隔 interval log 一行，直到 ctx 取消。pool 由呼叫端提供（取當下 pgxpool.Stat 的快照）
func Run(ctx context.Context, logger *slog.Logger, interval time.Duration, hists Hists, pool func() PoolSample) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	prev := pool()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cur := pool()
			line := BuildLine(prev, cur, hists.Create.drainOrEmpty(), now.Sub(last))
			line.Limiter = HistLine(hists.Limiter.drainOrEmpty())
			line.Publish = HistLine(hists.Publish.drainOrEmpty())
			logger.Info(LogMessage, "data", line)
			prev, last = cur, now
		}
	}
}
