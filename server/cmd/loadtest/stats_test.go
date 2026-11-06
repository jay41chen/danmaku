package main

import (
	"testing"
	"time"
)

func msec(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentile(t *testing.T) {
	// 1..100 ms：nearest-rank 的第 p 百分位就是 p ms
	var s []time.Duration
	for i := 1; i <= 100; i++ {
		s = append(s, msec(i))
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{50, msec(50)}, {95, msec(95)}, {99, msec(99)}, {100, msec(100)}, {1, msec(1)}, {99.5, msec(100)}} {
		if got := percentile(s, tc.p); got != tc.want {
			t.Errorf("percentile(1..100, %v) = %v, want %v", tc.p, got, tc.want)
		}
	}

	// 小樣本：不插值，回傳真的出現過的值
	small := []time.Duration{msec(10), msec(20), msec(30), msec(40)}
	if got := percentile(small, 50); got != msec(20) {
		t.Errorf("p50 of 4 = %v, want 20ms", got)
	}
	if got := percentile(small, 99); got != msec(40) {
		t.Errorf("p99 of 4 = %v, want 40ms", got)
	}
	if got := percentile([]time.Duration{msec(7)}, 99); got != msec(7) {
		t.Errorf("p99 of 1 = %v, want 7ms", got)
	}
	if got := percentile(nil, 99); got != 0 {
		t.Errorf("p99 of empty = %v, want 0", got)
	}
}

// 尖刺不能被平均掉：99 個 1ms + 1 個 1000ms
func TestSummarizeKeepsTheTail(t *testing.T) {
	var s []time.Duration
	for range 99 {
		s = append(s, msec(1))
	}
	s = append(s, msec(1000))
	// 故意打亂順序：summarize 要自己排序，而且不能改動呼叫端的 slice
	s[0], s[99] = s[99], s[0]
	before := append([]time.Duration(nil), s...)

	got := summarize(s)

	if got.N != 100 || got.P50Ms != 1 || got.P95Ms != 1 || got.P99Ms != 1 || got.MaxMs != 1000 {
		t.Errorf("summary = %+v, want p50=p95=p99=1 max=1000", got)
	}
	for i := range s {
		if s[i] != before[i] {
			t.Fatal("summarize modified its input")
		}
	}
	// 再多一個尖刺，p99 就要看得到
	s = append(s, msec(1000))
	if got := summarize(s); got.P99Ms != 1000 {
		t.Errorf("p99 with 2%% spikes = %v, want 1000", got.P99Ms)
	}
}
