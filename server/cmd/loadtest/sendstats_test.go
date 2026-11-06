package main

import (
	"net/http"
	"testing"
	"time"
)

// 手工組一個 open-loop 結果：每筆 (outcome, status, 延遲)
func fakeLoop(items ...struct {
	outcome int8
	status  int32
	lat     time.Duration
}) *openLoopResult {
	n := len(items)
	res := &openLoopResult{Interval: time.Millisecond, Total: n, FirstMeasured: 0,
		Outcome: make([]int8, n), Status: make([]int32, n), LatencyNs: make([]int64, n), LagNs: make([]int64, n)}
	for i, it := range items {
		res.Outcome[i], res.Status[i], res.LatencyNs[i] = it.outcome, it.status, int64(it.lat)
	}
	return res
}

type loopItem = struct {
	outcome int8
	status  int32
	lat     time.Duration
}

// timeout / 傳輸錯誤的延遲（下限）要算進 post_latency_all：
// 卡死的 server 會讓 8% 的請求 timeout，如果只算有回應的，p99 看起來很好
func TestSummarizeSendsIncludesErrorLatencies(t *testing.T) {
	var items []loopItem
	for range 92 {
		items = append(items, loopItem{outcomeResponse, http.StatusCreated, 20 * time.Millisecond})
	}
	for range 8 {
		items = append(items, loopItem{outcomeError, 0, 10 * time.Second}) // 等到 timeout 才放棄
	}
	s := summarizeSends(fakeLoop(items...))

	if s.Errors != 8 || s.Created != 92 {
		t.Fatalf("errors/created = %d/%d, want 8/92", s.Errors, s.Created)
	}
	if s.PostAll.N != 100 {
		t.Errorf("post_latency_all n = %d, want 100 (92 responses + 8 errors)", s.PostAll.N)
	}
	if s.PostAll.P99Ms < 10000 || s.PostAll.MaxMs < 10000 {
		t.Errorf("post_latency_all p99/max = %.0f/%.0fms, want the 10s timeouts to show up in the tail", s.PostAll.P99Ms, s.PostAll.MaxMs)
	}
	// 201 的分布不受影響：只算真的建立成功的
	if s.PostCreated.N != 92 || s.PostCreated.MaxMs > 25 {
		t.Errorf("post_latency_201 = %+v, want 92 samples all ~20ms", s.PostCreated)
	}
}
