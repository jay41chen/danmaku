package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// -scenario stuck：N 條 WS 連線 join 之後永遠不讀（也不回 ping），看 server 什麼時候、怎麼把它們清掉，以及 thread 數有沒有暴增。
// 不是吞吐量測試：沒有 CPU / 有效性判定，也沒有 sampled 連線。
// 整段期間（含 ramp）每秒讀一次 servers[0] 的 /metrics，量 websocket_connections_active、go_threads、process_resident_memory_bytes。

const (
	metricWSConnections = "websocket_connections_active"
	metricGoThreads     = "go_threads"
	metricRSSBytes      = "process_resident_memory_bytes"
)

// /metrics 裡 stuck 情境關心的三個值
type stuckMetrics struct {
	Connections int
	GoThreads   int
	RSSBytes    float64
}

// 從 /metrics 文字挑出三個 metric（這三個都沒有 label）；缺任何一個就回 error（訊息列出缺的名稱），不當成 0
func parseStuckMetrics(r io.Reader) (stuckMetrics, error) {
	vals, err := parsePlainMetrics(r, metricWSConnections, metricGoThreads, metricRSSBytes)
	if err != nil {
		return stuckMetrics{}, err
	}
	return stuckMetrics{Connections: int(vals[metricWSConnections]), GoThreads: int(vals[metricGoThreads]), RSSBytes: vals[metricRSSBytes]}, nil
}

// 從 /metrics 文字挑出指定名稱、沒有 label 的 metric；缺任何一個就回 error（訊息列出缺的名稱，依傳入順序），不當成 0
func parsePlainMetrics(r io.Reader, names ...string) (map[string]float64, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	vals := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || !want[fields[0]] {
			continue // 註解行（# HELP / # TYPE）、帶 label 的行都不是我們要的
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected metrics line for %s", fields[0])
		}
		vals[fields[0]] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range names {
		if _, ok := vals[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("/metrics is missing: %s", strings.Join(missing, ", "))
	}
	return vals, nil
}

// 一個時間點的兩種相對時間（秒）：相對第一條連線 join 成功、相對最後一條連上（ramp 結束）。還沒到那個時間點時是負的
type relTimes struct {
	SinceFirstConnectS float64 `json:"t_since_first_connect_s"`
	SinceAllConnectedS float64 `json:"t_since_all_connected_s"`
}

type stuckSample struct {
	relTimes
	Connections int     `json:"connections"` // server 的 websocket_connections_active
	GoThreads   int     `json:"go_threads"`
	RSSBytes    float64 `json:"rss_bytes"`
	Joined      int     `json:"tool_joined"` // 這一刻工具端已經 join 成功的條數，用來估被踢的數量

	at time.Time
}

type stuckSummary struct {
	Stuck           int     `json:"stuck"`
	StuckRate       float64 `json:"stuck_rate_msg_per_sec"`
	Joined          int     `json:"joined"`
	Rejected        int     `json:"rejected_joins"`
	RampStart       string  `json:"ramp_start"`
	FirstConnect    string  `json:"first_connect"`
	RampEnd         string  `json:"ramp_end"`
	ObservedSeconds float64 `json:"observed_seconds_after_all_connected"`

	BaselineConnections int     `json:"baseline_connections"`
	BaselineGoThreads   int     `json:"baseline_go_threads"`
	BaselineRSSBytes    float64 `json:"baseline_rss_bytes"`

	MaxGoThreads int      `json:"max_go_threads"`
	MaxThreadsAt relTimes `json:"max_go_threads_at"`
	MaxRSSBytes  float64  `json:"max_rss_bytes"`

	// 第一條被踢：估計的被踢數（基準 + 工具端 joined - server connections）第一次 >= 1。nil = 觀察期間沒發生。
	// 這是近似：server 的 gauge 在 upgrade 時就加、工具在 join_ok 後才算，in-flight 的 dial 會讓差值少算幾條
	FirstKick *relTimes `json:"first_kick,omitempty"`
	// 全部被踢完：最後一條連上之後，connections 第一次回到基準。nil = 沒有
	AllKicked *relTimes `json:"all_kicked,omitempty"`

	OpenAtEnd    int `json:"open_at_end"` // 觀察結束時 server 還開著的連線數（扣掉基準）
	SamplesTaken int `json:"samples_ok"`
	SampleErrors int `json:"sample_errors"`
	PostsSent    int `json:"posts_sent"`
	PostsCreated int `json:"posts_created_201"`
}

type stuckReport struct {
	Summary stuckSummary  `json:"summary"`
	Samples []stuckSample `json:"samples"`
}

type stuckInput struct {
	Stuck        int
	Rate         float64
	Baseline     stuckMetrics
	Samples      []stuckSample // 只有 at / 三個 metric / Joined 有值；相對時間由 analyzeStuck 填
	RampStart    time.Time
	FirstConnect time.Time // zero = 一條都沒連上
	AllConnected time.Time
	Joined       int
	Rejected     int
	SampleErrors int
}

// 算每個 sample 的相對時間，並找出最大 thread 數、第一條被踢、全部被踢完。純計算，沒有 I/O
func analyzeStuck(in stuckInput) stuckReport {
	rel := func(at time.Time) relTimes {
		r := relTimes{SinceAllConnectedS: at.Sub(in.AllConnected).Seconds()}
		if !in.FirstConnect.IsZero() {
			r.SinceFirstConnectS = at.Sub(in.FirstConnect).Seconds()
		}
		return r
	}
	sum := stuckSummary{
		Stuck: in.Stuck, StuckRate: in.Rate, Joined: in.Joined, Rejected: in.Rejected,
		BaselineConnections: in.Baseline.Connections, BaselineGoThreads: in.Baseline.GoThreads, BaselineRSSBytes: in.Baseline.RSSBytes,
		SamplesTaken: len(in.Samples), SampleErrors: in.SampleErrors,
		RampStart: in.RampStart.Format(time.RFC3339Nano), RampEnd: in.AllConnected.Format(time.RFC3339Nano),
	}
	if !in.FirstConnect.IsZero() {
		sum.FirstConnect = in.FirstConnect.Format(time.RFC3339Nano)
	}
	samples := make([]stuckSample, len(in.Samples))
	for i, s := range in.Samples {
		s.relTimes = rel(s.at)
		samples[i] = s
		if s.GoThreads > sum.MaxGoThreads {
			sum.MaxGoThreads, sum.MaxThreadsAt = s.GoThreads, s.relTimes
		}
		sum.MaxRSSBytes = max(sum.MaxRSSBytes, s.RSSBytes)
		if sum.FirstKick == nil && s.Joined > 0 && in.Baseline.Connections+s.Joined-s.Connections >= 1 {
			r := s.relTimes
			sum.FirstKick = &r
		}
		if sum.AllKicked == nil && s.Joined > 0 && !s.at.Before(in.AllConnected) && s.Connections <= in.Baseline.Connections {
			r := s.relTimes
			sum.AllKicked = &r
		}
	}
	if n := len(samples); n > 0 {
		sum.OpenAtEnd = max(samples[n-1].Connections-in.Baseline.Connections, 0)
		sum.ObservedSeconds = samples[n-1].SinceAllConnectedS
	}
	return stuckReport{Summary: sum, Samples: samples}
}

// 跑 stuck 情境：基準 → ramp（同時每秒取樣、必要時持續發彈幕）→ 從最後一條連上起觀察 -duration。
// 回傳 exit code（0 / 1 / 130）；資料清理由呼叫端 main.run 的 defer 負責
func runStuck(ctx context.Context, o *options, client *http.Client, servers []string, data *seeded, runTag string, rep *runReport, sc *scrubber) int {
	base := servers[0]
	var baseline stuckMetrics
	if err := fetchMetrics(ctx, client, base, func(r io.Reader) (err error) { baseline, err = parseStuckMetrics(r); return }); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest: "+sc.scrub("baseline /metrics: "+err.Error()))
		return 1
	}

	vp := newViewerPool(viewerConfig{
		Servers: servers[:1], Rooms: data.rooms[:1], Run: runTag, Sampled: 0,
		RampRate: o.rampRate, JoinTimeout: 10 * time.Second, MaxDialing: 128, Stuck: true,
	})
	defer vp.closeAll()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	rampStart := time.Now()
	in := stuckInput{Stuck: o.stuck, Rate: o.stuckRate, Baseline: baseline, RampStart: rampStart}

	// 每秒取樣：從 ramp 開始到觀察結束。第一筆就是 ramp 開始的這一刻
	var samples []stuckSample
	sampleErrs := 0
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			at := time.Now()
			joined := vp.aliveCount()
			var m stuckMetrics
			err := fetchMetrics(runCtx, client, base, func(r io.Reader) (err error) { m, err = parseStuckMetrics(r); return })
			if err != nil {
				if runCtx.Err() != nil {
					return
				}
				sampleErrs++
			} else {
				samples = append(samples, stuckSample{Connections: m.Connections, GoThreads: m.GoThreads, RSSBytes: m.RSSBytes, Joined: joined, at: at})
			}
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// 持續發彈幕：排程長度 = ramp 估計時間 + 觀察時間 + 餘裕，觀察結束時直接取消
	var loop *openLoopResult
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		if o.stuckRate <= 0 {
			return
		}
		p := &poster{client: client, servers: servers[:1], rooms: data.rooms[:1], tokens: data.tokens, run: runTag}
		rampEst := time.Duration(float64(o.stuck)/o.rampRate*float64(time.Second)) + 10*time.Second
		loop = runOpenLoop(runCtx, openLoopConfig{
			Start: time.Now().Add(200 * time.Millisecond), Rate: o.stuckRate,
			Measure: rampEst + o.duration + 30*time.Second, MaxInFlight: o.maxInFlight, Send: p.send,
		})
	}()

	fmt.Printf("stuck: %d connections, %.0f msg/s, observe %s after the last one connects\n", o.stuck, o.stuckRate, o.duration)
	ramp := vp.rampTo(ctx, o.stuck)
	in.AllConnected = time.Now()
	if ns := vp.firstJoinNs.Load(); ns != 0 {
		in.FirstConnect = time.Unix(0, ns)
	}
	in.Joined, in.Rejected = ramp.Joined, ramp.Rejected
	fmt.Printf("stuck: ramp done in %.1fs, joined %d, rejected %d (%s)\n", ramp.Seconds, ramp.Joined, ramp.Rejected, formatReasons(ramp.Reasons))
	sleepCtx(ctx, o.duration)

	cancelRun()
	<-sampleDone
	<-postDone
	in.Samples, in.SampleErrors = samples, sampleErrs
	report := analyzeStuck(in)
	if loop != nil {
		for i := range loop.Outcome {
			if loop.Outcome[i] == outcomeResponse || loop.Outcome[i] == outcomeError {
				report.Summary.PostsSent++
			}
			if loop.Outcome[i] == outcomeResponse && loop.Status[i] == http.StatusCreated {
				report.Summary.PostsCreated++
			}
		}
	}
	rep.Stuck = &report
	rep.RejectReasons = vp.reasons()
	vp.closeAll()
	if ctx.Err() != nil {
		rep.Aborted = true
		rep.Notes = append(rep.Notes, "interrupted: observation was cut short")
		return 130
	}
	return 0
}

// 給人看的摘要（完整的每秒資料在 JSON）
func printStuck(w io.Writer, r *stuckReport) {
	s := r.Summary
	secs := func(t *relTimes) string {
		if t == nil {
			return "never"
		}
		return fmt.Sprintf("%.1fs after first connect / %.1fs after all connected", t.SinceFirstConnectS, t.SinceAllConnectedS)
	}
	fmt.Fprintf(w, "\nscenario=stuck  stuck=%d joined=%d rate=%.0f msg/s  posts=%d (201: %d)\n", s.Stuck, s.Joined, s.StuckRate, s.PostsSent, s.PostsCreated)
	fmt.Fprintf(w, "baseline: connections=%d go_threads=%d rss=%.1f MB\n", s.BaselineConnections, s.BaselineGoThreads, s.BaselineRSSBytes/1e6)
	fmt.Fprintf(w, "go_threads max=%d at %s; rss max=%.1f MB\n", s.MaxGoThreads, secs(&s.MaxThreadsAt), s.MaxRSSBytes/1e6)
	fmt.Fprintf(w, "first kick: %s\nall kicked: %s\nopen at end: %d  (samples ok=%d errors=%d)\n", secs(s.FirstKick), secs(s.AllKicked), s.OpenAtEnd, s.SamplesTaken, s.SampleErrors)
}
