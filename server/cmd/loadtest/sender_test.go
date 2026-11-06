package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlanOpenLoop(t *testing.T) {
	interval, total, first := planOpenLoop(100, time.Second, 2*time.Second)
	if interval != 10*time.Millisecond || total != 300 || first != 100 {
		t.Errorf("plan = %v, %d, %d; want 10ms, 300, 100", interval, total, first)
	}
	_, _, first = planOpenLoop(10, 0, time.Second)
	if first != 0 {
		t.Errorf("no warm-up: first measured = %d, want 0", first)
	}
}

// 最重要的一個 test：coordinated omission 不能偷偷回來。
// 工具自己被拖慢（排程總是比預定時間晚 40ms 才輪到），送出本身是瞬間完成的。
// 如果延遲是從「實際開始送」量起，每一筆都只有幾微秒；從「排程時間」量起，每一筆至少要有 40ms
func TestLatencyIsMeasuredFromScheduledTimeNotActualSend(t *testing.T) {
	const lateness = 40 * time.Millisecond
	slowWait := func(ctx context.Context, at time.Time) { realWaitUntil(ctx, at.Add(lateness)) }

	res := runOpenLoop(context.Background(), openLoopConfig{
		Start: time.Now(), Rate: 100, Warmup: 0, Measure: 300 * time.Millisecond, MaxInFlight: 100,
		Send:      func(ctx context.Context, seq int) (int, error) { return 201, nil },
		WaitUntil: slowWait,
	})

	sum := summarizeSends(res)
	if sum.Created != 30 {
		t.Fatalf("created = %d, want 30", sum.Created)
	}
	if sum.PostAll.P50Ms < 0.9*float64(lateness/time.Millisecond) {
		t.Errorf("POST p50 = %.1fms, want >= ~40ms: latency must include the time the tool was late", sum.PostAll.P50Ms)
	}
	if sum.Lag.P50Ms < 0.9*float64(lateness/time.Millisecond) {
		t.Errorf("scheduling lag p50 = %.1fms, want >= ~40ms", sum.Lag.P50Ms)
	}
}

// 慢 server：每筆要 150ms，但每 10ms 排程一筆。每一筆的延遲都是自己的 150ms（不是排隊等前一筆），
// 而且排程不受影響（open-loop：lag 很小），也沒有 dropped
func TestSlowSenderDoesNotSlowTheSchedule(t *testing.T) {
	var started atomic.Int64
	res := runOpenLoop(context.Background(), openLoopConfig{
		Start: time.Now(), Rate: 100, Warmup: 0, Measure: 500 * time.Millisecond, MaxInFlight: 1000,
		Send: func(ctx context.Context, seq int) (int, error) {
			started.Add(1)
			time.Sleep(150 * time.Millisecond)
			return 201, nil
		},
	})
	sum := summarizeSends(res)
	if sum.Dropped != 0 || sum.Created != 50 {
		t.Fatalf("dropped=%d created=%d, want 0 and 50", sum.Dropped, sum.Created)
	}
	if sum.PostAll.P50Ms < 140 {
		t.Errorf("POST p50 = %.1fms, want >= 150ms service time", sum.PostAll.P50Ms)
	}
	// 如果是 closed-loop（等前一筆回來才送下一筆），500ms 內只能送 ~4 筆，而且最後一筆的排程延遲會高達數百 ms
	if sum.Lag.P99Ms > 30 {
		t.Errorf("scheduling lag p99 = %.1fms: a slow sender must not delay the schedule", sum.Lag.P99Ms)
	}
	if started.Load() != 50 {
		t.Errorf("sends started = %d, want 50", started.Load())
	}
}

// 超過 in-flight 上限的筆數要記成 dropped（不是默默等、也不是默默不算），而且 validity 會因此失敗
func TestInFlightCapCountsDroppedAndInvalidates(t *testing.T) {
	res := runOpenLoop(context.Background(), openLoopConfig{
		Start: time.Now(), Rate: 200, Warmup: 0, Measure: 500 * time.Millisecond, MaxInFlight: 5,
		Send: func(ctx context.Context, seq int) (int, error) {
			time.Sleep(200 * time.Millisecond)
			return 201, nil
		},
	})
	sum := summarizeSends(res)
	if sum.Dropped == 0 {
		t.Fatal("dropped = 0, want some: cap of 5 with 200ms service at 200/s must overflow")
	}
	if sum.Created+sum.Dropped != sum.Measured {
		t.Errorf("created %d + dropped %d != measured %d", sum.Created, sum.Dropped, sum.Measured)
	}
	reasons := evaluateValidity(validityInput{Measured: sum.Measured, Dropped: sum.Dropped, LagThreshold: time.Second})
	if len(reasons) == 0 {
		t.Error("a step with dropped sends was reported valid")
	}
}

// 暖機的送出不進統計；429 與傳輸錯誤分開計
func TestWarmupExcludedAndStatusesCountedSeparately(t *testing.T) {
	res := runOpenLoop(context.Background(), openLoopConfig{
		Start: time.Now(), Rate: 100, Warmup: 100 * time.Millisecond, Measure: 200 * time.Millisecond, MaxInFlight: 100,
		Send: func(ctx context.Context, seq int) (int, error) {
			switch {
			case seq < 10: // 暖機期間全部 500：不能出現在統計裡
				return 500, nil
			case seq%10 == 0:
				return 429, nil
			case seq%10 == 1:
				return 0, context.DeadlineExceeded
			}
			return 201, nil
		},
	})
	sum := summarizeSends(res)
	if sum.Measured != 20 || res.FirstMeasured != 10 {
		t.Fatalf("measured = %d first = %d, want 20 and 10", sum.Measured, res.FirstMeasured)
	}
	if sum.RateLimited != 2 || sum.Errors != 2 || sum.Created != 16 {
		t.Errorf("429=%d errors=%d created=%d; want 2, 2, 16", sum.RateLimited, sum.Errors, sum.Created)
	}
	if sum.StatusCounts["Internal Server Error 500"] != 0 {
		t.Error("warm-up 500s leaked into the statistics")
	}
}

func TestCancelledRunStopsScheduling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res := runOpenLoop(ctx, openLoopConfig{
		Start: time.Now(), Rate: 100, Measure: 10 * time.Second, MaxInFlight: 10,
		Send: func(ctx context.Context, seq int) (int, error) {
			if seq == 3 {
				cancel()
			}
			return 201, nil
		},
	})
	sum := summarizeSends(res)
	if sum.NotRun < 900 {
		t.Errorf("not_run = %d, want most of the 1000 scheduled sends to be skipped", sum.NotRun)
	}
}

// 固定速率：第 i 筆不能早於 Start + i*interval 送出（open-loop 的排程要真的照時間表走，不是一股腦送完）
func TestSendsFollowTheScheduleAtAFixedRate(t *testing.T) {
	start := time.Now().Add(20 * time.Millisecond)
	launched := make([]time.Duration, 20)
	runOpenLoop(context.Background(), openLoopConfig{
		Start: start, Rate: 100, Measure: 200 * time.Millisecond, MaxInFlight: 100,
		Send: func(ctx context.Context, seq int) (int, error) {
			launched[seq] = time.Since(start)
			return 201, nil
		},
	})
	for seq, at := range launched {
		if early := time.Duration(seq)*10*time.Millisecond - at; early > 2*time.Millisecond {
			t.Errorf("send %d launched %v before its scheduled time", seq, early)
		}
	}
	if last := launched[len(launched)-1]; last < 180*time.Millisecond {
		t.Errorf("last send at %v, want ~190ms: sends were not paced", last)
	}
}
