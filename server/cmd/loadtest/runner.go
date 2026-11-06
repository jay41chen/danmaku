package main

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// 一個 step 的執行流程：補連線 → 靜置 → open-loop 送出（先暖機，再量測）→ 等漏收的訊息到齊 → 彙整

type runnerConfig struct {
	Run          string
	Rooms        []string
	Servers      []string
	Tokens       []string
	Sampled      int
	Settle       time.Duration
	Warmup       time.Duration
	Measure      time.Duration
	Drain        time.Duration
	MaxInFlight  int
	LagThreshold time.Duration
	ToolCPUFrac  float64
	CollapseP99  time.Duration

	// 整台機器飽和判斷用：機器的 core 數、要求量到的 server 程序數與 container 數（沒給 = 判斷不了）
	NumCPU        int
	ServerPIDs    int
	DockerTargets int

	// 其他程序用掉的 core 數達這個值就判 step 無效；<= 0 = 不檢查
	OtherCoresLimit float64

	// 整機 / other 判斷不了時，step 直接無效（-require-machine-check）
	RequireMachineCheck bool
}

type runner struct {
	cfg    runnerConfig
	pool   *viewerPool
	mon    *monitor
	client *poster
}

// 睡到指定時間點，ctx 取消就提早回來
func sleepCtx(ctx context.Context, d time.Duration) {
	realWaitUntil(ctx, time.Now().Add(d))
}

// 執行一個 step。ctx 取消時回傳 Status=aborted 的結果（已量到的部分照樣保留）
func (r *runner) runStep(ctx context.Context, idx int, spec stepSpec) stepResult {
	cfg := r.cfg
	res := stepResult{Index: idx, Spec: spec, Status: "ran"}
	target := max(spec.Viewers, cfg.Sampled)
	res.Viewers.Target = target

	res.Ramp = r.pool.rampTo(ctx, target)
	sleepCtx(ctx, cfg.Settle)
	if ctx.Err() != nil {
		return abortedStep(res)
	}

	interval, total, _ := planOpenLoop(spec.Rate, cfg.Warmup, cfg.Measure)
	start := time.Now().Add(200 * time.Millisecond)
	measureStart := start.Add(cfg.Warmup)
	measureEnd := start.Add(time.Duration(total) * interval)

	joined, sampleRooms := r.pool.sampleState(cfg.Sampled)
	rec := newStepRecorder(idx, start, cfg.Sampled, total)
	r.pool.recorder.Store(rec)
	defer r.pool.recorder.Store(nil)
	closes0 := r.pool.closes.Load()

	p := *r.client
	p.step = idx
	p.rooms = cfg.Rooms[:max(spec.Rooms, 1)]
	loopDone := make(chan *openLoopResult, 1)
	go func() {
		loopDone <- runOpenLoop(ctx, openLoopConfig{
			Start: start, Rate: spec.Rate, Warmup: cfg.Warmup, Measure: cfg.Measure,
			MaxInFlight: cfg.MaxInFlight, Send: p.send,
		})
	}()

	// 量測窗口起點：暖機結束的那一刻
	realWaitUntil(ctx, measureStart)
	t0, cpu0, frames0 := time.Now(), selfCPUSeconds(), r.pool.totalFrames()
	res.Viewers.AliveAtStart = r.pool.aliveCount()
	res.LoadAvg.Start, _ = readLoadAvg()
	psFrom := r.mon.snapshotPS(ctx)
	handler0, handlerErrs0 := snapshotAllHandlers(ctx, r.client.client, cfg.Servers)

	// 量測窗口終點：最後一筆排程時間之後
	realWaitUntil(ctx, measureEnd)
	t1, cpu1, frames1 := time.Now(), selfCPUSeconds(), r.pool.totalFrames()
	res.LoadAvg.End, _ = readLoadAvg()
	r.mon.snapshotPS(ctx)
	handler1, handlerErrs1 := snapshotAllHandlers(ctx, r.client.client, cfg.Servers)
	for base, err := range handlerErrs1 {
		handlerErrs0[base] = err
	}
	res.Handler = summarizeHandler(cfg.Servers, handler0, handler1, handlerErrs0)

	loop := <-loopDone
	// 等 server 把最後幾筆廣播送到 sampled 連線；不能省，否則尾端的訊息會被誤判成漏收
	sleepCtx(ctx, cfg.Drain)
	res.Viewers.ClosesAfterJoin = int(r.pool.closes.Load() - closes0)
	r.pool.recorder.Store(nil)

	if ctx.Err() != nil {
		res.Status = "aborted"
	}

	wall := t1.Sub(t0).Seconds()
	res.MeasureSeconds = wall
	res.windowFrom, res.windowTo = t0, t1
	res.Send = summarizeSends(loop)
	res.Frames = framesResult{Total: frames1 - frames0}
	if wall > 0 {
		res.Frames.PerSec = float64(res.Frames.Total) / wall
		res.CPU.ToolCores = (cpu1 - cpu0) / wall
	}
	res.CPU.ToolLimitCores = cfg.ToolCPUFrac * float64(runtime.GOMAXPROCS(0))
	res.CPU.Resources = r.mon.window(psFrom, t1)

	for _, ok := range joined {
		if ok {
			res.Viewers.SampledJoined++
		}
	}
	res.Receive = summarizeReceive(sampleInput{
		Interval: loop.Interval, FirstMeasured: loop.FirstMeasured, Total: loop.Total,
		Joined:   joined,
		Expected: expectedReceive(loop, len(p.rooms), sampleRooms),
		Received: rec.received,
	})

	return finishStep(res, cfg)
}

// 哪些 (sampled 連線, seq) 應該收到：只有 server 回 201 的（429 / 5xx / 錯誤 / dropped 都沒有廣播），
// 而且 seq 的房間要是這條連線所在的房間
func expectedReceive(loop *openLoopResult, rooms int, sampleRooms []int) func(sample, seq int) bool {
	return func(sample, seq int) bool {
		return loop.Outcome[seq] == outcomeResponse && loop.Status[seq] == http.StatusCreated &&
			roomIndex(seq, rooms) == sampleRooms[sample]
	}
}

func abortedStep(res stepResult) stepResult {
	res.Status = "aborted"
	res.Valid = false
	res.InvalidReasons = []string{"run was interrupted before this step was measured"}
	return res
}

// 套用 validity 與 collapse 規則、產生警告。純計算
func finishStep(res stepResult, cfg runnerConfig) stepResult {
	in := validityInput{
		Measured:        res.Send.Measured,
		LagP99:          time.Duration(res.Send.Lag.P99Ms * float64(time.Millisecond)),
		LagThreshold:    cfg.LagThreshold,
		Dropped:         res.Send.Dropped,
		RateLimited:     res.Send.RateLimited,
		ToolCPUCores:    res.CPU.ToolCores,
		ToolCPULimitCPU: res.CPU.ToolLimitCores,
		ViewersTarget:   res.Viewers.Target,
		ViewersJoined:   res.Viewers.AliveAtStart,
		Failed:          res.Send.Errors + res.Send.Responses - res.Send.Created,
	}
	res.CPU.MachineCores, res.CPU.MachineJudged, res.CPU.MachineNote = machineLoad(res.CPU.ToolCores, res.CPU.Resources, cfg.ServerPIDs, cfg.DockerTargets)
	if cfg.NumCPU <= 0 && res.CPU.MachineJudged {
		res.CPU.MachineJudged, res.CPU.MachineNote = false, "machine core count unknown"
	}
	in.MachineCores, in.MachineNumCPU, in.MachineJudged = res.CPU.MachineCores, cfg.NumCPU, res.CPU.MachineJudged
	res.CPU.TotalCores, res.CPU.OtherCores, res.CPU.TotalJudged, res.CPU.OtherJudged, res.CPU.OtherNote =
		machineWide(res.CPU.Resources, res.CPU.MachineCores, res.CPU.MachineJudged)
	if cfg.NumCPU <= 0 && res.CPU.TotalJudged {
		res.CPU.TotalJudged, res.CPU.OtherJudged, res.CPU.OtherNote = false, false, "machine core count unknown"
	}
	in.RequireMachineCheck = cfg.RequireMachineCheck
	in.JudgeNote = res.CPU.OtherNote
	if in.JudgeNote == "" {
		in.JudgeNote = res.CPU.MachineNote
	}
	in.TotalCores, in.TotalJudged, in.OtherCores, in.OtherJudged, in.OtherLimit =
		res.CPU.TotalCores, res.CPU.TotalJudged, res.CPU.OtherCores, res.CPU.OtherJudged, cfg.OtherCoresLimit
	res.Frames.Expected = expectedFrames(res.Viewers.AliveAtStart, res.Spec.Rate, res.Spec.Rooms)
	if res.Frames.Expected > 0 {
		res.Frames.Ratio = res.Frames.PerSec / res.Frames.Expected
	}
	res.InvalidReasons = evaluateValidity(in)
	if res.Status == "aborted" {
		res.InvalidReasons = append(res.InvalidReasons, "run was interrupted during this step")
	}
	res.Valid = len(res.InvalidReasons) == 0

	if res.Status == "aborted" {
		// 被中斷的 step 資料不完整，不判斷崩潰
		return withWarnings(res)
	}
	res.Collapsed, res.CollapseReason = evaluateCollapse(collapseInput{
		Measured:      res.Send.Measured,
		Succeeded:     res.Send.Created,
		PostP99:       time.Duration(res.Send.PostAll.P99Ms * float64(time.Millisecond)),
		P99Limit:      cfg.CollapseP99,
		SampleMissing: res.Receive.Missing,
		SampleExpect:  res.Receive.Expected,
		Disconnects:   res.Viewers.ClosesAfterJoin,
		ViewersJoined: res.Viewers.AliveAtStart,
	})
	return withWarnings(res)
}

// 依統計補上警告（不影響 valid）
func withWarnings(res stepResult) stepResult {
	if res.Status == "ran" && !res.CPU.MachineJudged {
		res.Warnings = append(res.Warnings, "whole-machine CPU saturation could NOT be judged: "+res.CPU.MachineNote)
	}
	if res.Status == "ran" && !res.CPU.TotalJudged {
		res.Warnings = append(res.Warnings, "machine idleness (other programs' CPU) could NOT be judged: "+res.CPU.OtherNote)
	} else if res.Status == "ran" && !res.CPU.OtherJudged {
		res.Warnings = append(res.Warnings, "other programs' CPU (other_cores) could NOT be derived: "+res.CPU.OtherNote)
	}
	if res.Send.Errors > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d POSTs failed without a response (timeout or connection error)", res.Send.Errors))
	}
	if n := res.Send.Responses - res.Send.Created - res.Send.RateLimited; n > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d POST responses were neither 201 nor 429 (see status_counts)", n))
	}
	if res.Receive.Missing > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d of %d expected messages never reached the sampled connections", res.Receive.Missing, res.Receive.Expected))
	}
	if res.Viewers.ClosesAfterJoin > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d connections closed after join during this step", res.Viewers.ClosesAfterJoin))
	}
	if res.Ramp.Rejected > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d joins were rejected while ramping up", res.Ramp.Rejected))
	}
	if res.Status == "ran" && res.Receive.Samples == 0 {
		res.Warnings = append(res.Warnings, "no sampled connection was in the room: send-to-receive latency was not measured")
	}
	return res
}
