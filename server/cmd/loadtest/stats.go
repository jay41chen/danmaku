package main

import (
	"math"
	"slices"
	"strconv"
	"time"
)

// 延遲摘要，單位毫秒（JSON 與表格都用 ms）
type latencySummary struct {
	N     int     `json:"n"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

// 取已排序資料的第 p 百分位（nearest-rank：第 ceil(p/100*n) 筆）。p 在 (0, 100]；沒資料回 0
// 不插值：壓測要的是「實際出現過的值」，插值會造出沒人真的遇過的延遲
func percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	rank = min(max(rank, 1), n)
	return sorted[rank-1]
}

func durMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// 算 p50 / p95 / p99 / max；不改動傳進來的 slice
func summarize(samples []time.Duration) latencySummary {
	if len(samples) == 0 {
		return latencySummary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return latencySummary{
		N:     len(sorted),
		P50Ms: durMs(percentile(sorted, 50)),
		P95Ms: durMs(percentile(sorted, 95)),
		P99Ms: durMs(percentile(sorted, 99)),
		MaxMs: durMs(sorted[len(sorted)-1]),
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
