package main

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Open-loop 送出：用固定速率的「排程時間」決定每一筆什麼時候該送，不等前一筆回應。
// 延遲一律從「排程時間」量到回應（不是從實際送出的時間），工具自己慢了、或 server 慢到卡住請求，
// 都會算進延遲，不會因為送得慢而少看到慢的樣本（coordinated omission）。

// 每一筆送出的結果
const (
	outcomeNotRun   int8 = iota // 沒執行（被取消）
	outcomeResponse             // 收到 HTTP 回應（任何 status）
	outcomeError                // 傳輸錯誤 / timeout，沒有 status
	outcomeDropped              // 超過 in-flight 上限，沒送
)

// 送出一筆（seq 從 0 開始）；回傳 HTTP status，傳輸失敗回 error
type sendFunc func(ctx context.Context, seq int) (status int, err error)

type openLoopConfig struct {
	Start       time.Time // 第 0 筆的排程時間（單調時鐘，用 time.Now() 派生）
	Rate        float64   // 每秒幾筆
	Warmup      time.Duration
	Measure     time.Duration
	MaxInFlight int
	Send        sendFunc
	// 等到指定時間點；nil = 真的睡到那個時間。測試可以換成故意遲到的版本，模擬工具自己被拖慢
	WaitUntil func(ctx context.Context, t time.Time)
	// 現在時間；nil = time.Now。測試用
	Now func() time.Time
}

// Open-loop 的結果：全部以 seq 為索引
type openLoopResult struct {
	Interval      time.Duration // 兩筆之間的排程間隔
	Total         int           // 排程的總筆數（含暖機）
	FirstMeasured int           // 第一筆量測的 seq；之前的是暖機，不進統計
	Outcome       []int8
	Status        []int32
	LatencyNs     []int64 // 排程時間 → 回應完成
	LagNs         []int64 // 排程時間 → 實際開始送（工具自己的排程延遲）
}

// 排程時間相對於 Start 的位移
func (r *openLoopResult) schedOffset(seq int) time.Duration {
	return time.Duration(seq) * r.Interval
}

func realWaitUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// 計算排程：筆數與暖機的分界。獨立出來讓測試能直接驗
func planOpenLoop(rate float64, warmup, measure time.Duration) (interval time.Duration, total, firstMeasured int) {
	interval = time.Duration(float64(time.Second) / rate)
	total = int(math.Round((warmup + measure).Seconds() * rate))
	firstMeasured = int((warmup + interval - 1) / interval)
	return interval, total, firstMeasured
}

// 執行 open-loop 送出，回傳時所有已送出的請求都已結束。
// 每一筆在自己的 goroutine 送，所以前一筆卡住不影響後一筆的排程；超過 MaxInFlight 的那筆不送、記為 dropped
// ctx 被取消時停止排程，沒執行的筆數維持 outcomeNotRun
func runOpenLoop(ctx context.Context, cfg openLoopConfig) *openLoopResult {
	wait := cfg.WaitUntil
	if wait == nil {
		wait = realWaitUntil
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval, total, first := planOpenLoop(cfg.Rate, cfg.Warmup, cfg.Measure)
	res := &openLoopResult{
		Interval:      interval,
		Total:         total,
		FirstMeasured: first,
		Outcome:       make([]int8, total),
		Status:        make([]int32, total),
		LatencyNs:     make([]int64, total),
		LagNs:         make([]int64, total),
	}

	var inFlight atomic.Int64
	var wg sync.WaitGroup
	for seq := range total {
		// 絕對排程時間：落後了就連續補送，不會把落後的時間吃掉（那會把負載悄悄降低）
		sched := cfg.Start.Add(time.Duration(seq) * interval)
		wait(ctx, sched)
		if ctx.Err() != nil {
			break
		}
		res.LagNs[seq] = max(int64(now().Sub(sched)), 0)

		if inFlight.Add(1) > int64(cfg.MaxInFlight) {
			inFlight.Add(-1)
			res.Outcome[seq] = outcomeDropped
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer inFlight.Add(-1)
			status, err := cfg.Send(ctx, seq)
			// 延遲從排程時間算，不是從這個 goroutine 開始的時間
			res.LatencyNs[seq] = max(int64(now().Sub(sched)), 0)
			if err != nil {
				res.Outcome[seq] = outcomeError
				return
			}
			res.Status[seq] = int32(status)
			res.Outcome[seq] = outcomeResponse
		}()
	}
	wg.Wait()
	return res
}
