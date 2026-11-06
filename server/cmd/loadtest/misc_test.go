package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/loadtiming"
)

func TestMarkerRoundTrip(t *testing.T) {
	step, seq, ok := parseMarker("ab12", makeMarker("ab12", 3, 4567))
	if !ok || step != 3 || seq != 4567 {
		t.Errorf("round trip = %d %d %v", step, seq, ok)
	}
	for _, c := range []string{"hello", "lt-other-1-2", "lt-ab12-x-2", "lt-ab12-1", "lt-ab12-1-y", "lt-ab12--1-2", ""} {
		if _, _, ok := parseMarker("ab12", c); ok {
			t.Errorf("parseMarker accepted %q", c)
		}
	}
	// 長度遠低於 server 的 100 字元內容上限
	if n := len(makeMarker("ab12cd34", 9, 999999)); n > 40 {
		t.Errorf("marker length %d is too long", n)
	}
}

// send-to-receive 延遲是 收到時間 - 排程時間；漏收的算 missing；不在房間的 sample 不算 expected
func TestSummarizeReceive(t *testing.T) {
	interval := 10 * time.Millisecond
	recv := map[[2]int]time.Duration{
		// sample 0 收到 seq 2..4，分別比排程晚 5 / 15 / 25 ms；沒收到 seq 5
		{0, 2}: 2*interval + 5*time.Millisecond,
		{0, 3}: 3*interval + 15*time.Millisecond,
		{0, 4}: 4*interval + 25*time.Millisecond,
		// sample 1 全收到，晚 1ms
		{1, 2}: 2*interval + time.Millisecond, {1, 3}: 3*interval + time.Millisecond,
		{1, 4}: 4*interval + time.Millisecond, {1, 5}: 5*interval + time.Millisecond,
	}
	got := summarizeReceive(sampleInput{
		Interval: interval, FirstMeasured: 2, Total: 6,
		Joined:   []bool{true, true, false},
		Expected: func(sample, seq int) bool { return true },
		Received: func(sample, seq int) (time.Duration, bool) { d, ok := recv[[2]int{sample, seq}]; return d, ok },
	})
	if got.Samples != 2 || got.Expected != 8 || got.Received != 7 || got.Missing != 1 {
		t.Errorf("counts = %+v, want samples 2 expected 8 received 7 missing 1", got)
	}
	if got.Latency.MaxMs != 25 || got.Latency.N != 7 {
		t.Errorf("latency = %+v, want max 25ms over 7 samples", got.Latency)
	}
	// 暖機期間（seq < FirstMeasured）完全不算：Total 6、FirstMeasured 2 → 每個 sample 只有 4 筆 expected（上面的 8 = 2 x 4）
}

func TestRecorderKeepsFirstReceive(t *testing.T) {
	start := time.Now()
	rec := newStepRecorder(1, start, 2, 10)
	rec.record(0, 3, start.Add(50*time.Millisecond))
	rec.record(0, 3, start.Add(90*time.Millisecond)) // 重複：保留第一次
	rec.record(5, 3, start)                          // 超出範圍：忽略，不 panic
	rec.record(0, 99, start)
	if off, ok := rec.received(0, 3); !ok || off != 50*time.Millisecond {
		t.Errorf("received = %v %v, want 50ms", off, ok)
	}
	if _, ok := rec.received(1, 3); ok {
		t.Error("sample 1 reported a message it never received")
	}
}

func TestParsePSTime(t *testing.T) {
	for in, want := range map[string]float64{
		"0:01.50":    1.5,
		"12:34.25":   754.25,
		"1:02:03.5":  3723.5,
		"00:00:07":   7,
		"1-02:03:04": 86400 + 3600*2 + 180 + 4,
	} {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("parsePSTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "1:xx"} {
		if _, err := parsePSTime(in); err == nil {
			t.Errorf("parsePSTime(%q) = nil error", in)
		}
	}
	if v, err := parseDockerCPU("12.50%"); err != nil || v != 12.5 {
		t.Errorf("parseDockerCPU = %v %v", v, err)
	}
}

func TestParseStepsAndBuildSteps(t *testing.T) {
	steps, err := parseSteps("100x10, 500x20.5")
	if err != nil || len(steps) != 2 || steps[1].Viewers != 500 || steps[1].Rate != 20.5 {
		t.Fatalf("parseSteps = %+v, %v", steps, err)
	}
	for _, bad := range []string{"", "100", "x10", "100x0", "100x-1", "ax1", "100x"} {
		if _, err := parseSteps(bad); err == nil {
			t.Errorf("parseSteps(%q) = nil error", bad)
		}
	}

	o := &options{scenario: "rooms", rooms: 500, perRoom: 10, roomRate: 1}
	got, err := buildSteps(o)
	if err != nil || len(got) != 1 || got[0].Viewers != 5000 || got[0].Rate != 500 || got[0].Rooms != 500 {
		t.Errorf("rooms steps = %+v, %v", got, err)
	}
	o = &options{scenario: "fanout", steps: defaultSteps, fanoutRate: 5}
	got, _ = buildSteps(o)
	if len(got) != 5 || got[4].Viewers != 5000 || got[0].Rate != 5 || got[4].Rate != 5 {
		t.Errorf("fanout steps = %+v", got)
	}
	o = &options{scenario: "mixed", steps: defaultSteps}
	got, _ = buildSteps(o)
	if len(got) != 5 || got[3].Viewers != 2500 || got[3].Rate != 100 {
		t.Errorf("mixed default steps = %+v", got)
	}
	o = &options{scenario: "write", writeRate: 100}
	if got, _ := buildSteps(o); len(got) != 1 || got[0].Rate != 100 || got[0].Viewers != 0 {
		t.Errorf("write steps = %+v", got)
	}
	if _, err := buildSteps(&options{scenario: "nope"}); err == nil {
		t.Error("unknown scenario accepted")
	}
}

// 計時行的挑選：區間與窗口重疊至少一半才算；bucket 合併後算 p50 / p99
func TestSummarizeTiming(t *testing.T) {
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	mk := func(endOffset time.Duration, count int64, bucketMs float64) timingRecord {
		b := make([]int64, len(loadtiming.BoundsMs)+1)
		for i, bound := range loadtiming.BoundsMs {
			if bound == bucketMs {
				b[i] = count
			}
		}
		return timingRecord{Time: base.Add(endOffset), Msg: loadtiming.LogMessage, Data: loadtiming.Line{
			IntervalMs: 5000,
			Pool:       loadtiming.PoolLine{AcquireCount: count, EmptyAcquireCount: 1, Acquired: 3, Idle: 7, Max: 10},
			Create:     loadtiming.CreateLine{Count: count, SumMs: float64(count) * bucketMs, BoundsMs: loadtiming.BoundsMs, Buckets: b},
		}}
	}
	recs := []timingRecord{
		mk(5*time.Second, 1000, 1),    // [0,5] 在窗口 [10,20] 外
		mk(12*time.Second, 1000, 2),   // [7,12] 與窗口重疊 2s < 2.5s → 不採用
		mk(15*time.Second, 1000, 5),   // [10,15] 採用
		mk(20*time.Second, 1000, 5),   // [15,20] 採用
		mk(25*time.Second, 1000, 100), // [20,25] 在窗口外
	}
	s := summarizeTiming("x.log", recs, base.Add(10*time.Second), base.Add(20*time.Second))
	if s.Lines != 2 || s.CreateCount != 2000 || s.AcquireCount != 2000 || s.EmptyAcquire != 2 {
		t.Errorf("summary = %+v, want 2 lines / 2000 creates", s)
	}
	if s.CreateP50Ms != 5 || s.CreateP99Ms != 5 {
		t.Errorf("p50 = %v p99 = %v, want 5 / 5", s.CreateP50Ms, s.CreateP99Ms)
	}
	if s.AcquiredMax != 3 || s.IdleMin != 7 || s.MaxConns != 10 {
		t.Errorf("gauges = %+v", s)
	}
	// acquire_mean = acquire_total / 全部 acquires；mk 沒設 AcquireDurationMs，所以是 0，query_mean = create_mean
	if s.AcquireMeanMs != 0 || s.QueryMeanMs != s.CreateMeanMs || s.CreateMeanMs != 5 {
		t.Errorf("acquire/query/create mean = %v/%v/%v", s.AcquireMeanMs, s.QueryMeanMs, s.CreateMeanMs)
	}
	empty := summarizeTiming("x.log", nil, base, base.Add(time.Second))
	if empty.Lines != 0 || len(empty.Notes) == 0 {
		t.Errorf("empty summary = %+v, want a note", empty)
	}
}

func TestReadTimingLinesFiltersOtherLogs(t *testing.T) {
	line, _ := json.Marshal(map[string]any{"time": "2026-11-06T12:00:05Z", "level": "INFO", "msg": loadtiming.LogMessage,
		"data": loadtiming.Line{IntervalMs: 5000, Create: loadtiming.CreateLine{Count: 7}}})
	content := `{"time":"2026-11-06T12:00:00Z","msg":"request","path":"/loadtest_timing_decoy"}` + "\n" +
		string(line) + "\n" +
		`{"time":"2026-11-06T12:00:10Z","msg":"loadtest_timing","data":{` + "\n" // 寫到一半
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := readTimingLines(path)
	if err != nil || len(recs) != 1 || recs[0].Data.Create.Count != 7 {
		t.Errorf("recs = %+v, err = %v; want exactly the one complete timing line", recs, err)
	}
}

// acquire_mean = acquire_total / 全部 acquires（acquire_total 含全部）；query_mean = create_mean - acquire_mean；limiter / publish 平均各自獨立
func TestSummarizeTimingBreakdown(t *testing.T) {
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b := make([]int64, len(loadtiming.BoundsMs)+1)
	rec := timingRecord{Time: base.Add(5 * time.Second), Msg: loadtiming.LogMessage, Data: loadtiming.Line{
		IntervalMs: 5000,
		Pool:       loadtiming.PoolLine{AcquireCount: 100, AcquireDurationMs: 40, EmptyAcquireCount: 10},
		Create:     loadtiming.CreateLine{Count: 100, SumMs: 600, BoundsMs: loadtiming.BoundsMs, Buckets: b},
		Limiter:    loadtiming.CreateLine{Count: 200, Errors: 1, SumMs: 100},
		Publish:    loadtiming.CreateLine{Count: 100, SumMs: 250},
	}}
	s := summarizeTiming("x.log", []timingRecord{rec}, base, base.Add(5*time.Second))
	if s.AcquireMeanMs != 0.4 || s.CreateMeanMs != 6 || s.QueryMeanMs != 5.6 {
		t.Errorf("acquire/create/query mean = %v/%v/%v, want 0.4/6/5.6", s.AcquireMeanMs, s.CreateMeanMs, s.QueryMeanMs)
	}
	if s.LimiterCount != 200 || s.LimiterErrors != 1 || s.LimiterMeanMs != 0.5 || s.PublishCount != 100 || s.PublishMeanMs != 2.5 {
		t.Errorf("limiter %d/%d/%v publish %d/%v", s.LimiterCount, s.LimiterErrors, s.LimiterMeanMs, s.PublishCount, s.PublishMeanMs)
	}
}

// 真實資料的形狀：acquire 很多、empty 很少（每次 empty 要等 60 ms 左右）。舊算法用 empty 當分母會得到 acquire_mean 60.53 ms、
// query_mean 為負；現在分母是全部 acquires，query_mean 必須是正的
func TestSummarizeTimingManyAcquiresFewEmpties(t *testing.T) {
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b := make([]int64, len(loadtiming.BoundsMs)+1)
	rec := timingRecord{Time: base.Add(5 * time.Second), Msg: loadtiming.LogMessage, Data: loadtiming.Line{
		IntervalMs: 5000,
		Pool:       loadtiming.PoolLine{AcquireCount: 100000, AcquireDurationMs: 3026.5, EmptyAcquireCount: 50},
		Create:     loadtiming.CreateLine{Count: 100000, SumMs: 320000, BoundsMs: loadtiming.BoundsMs, Buckets: b}, // mean 3.2 ms
	}}
	s := summarizeTiming("x.log", []timingRecord{rec}, base, base.Add(5*time.Second))
	if math.Abs(s.AcquireMeanMs-0.030265) > 1e-9 || math.Abs(s.CreateMeanMs-3.2) > 1e-9 || math.Abs(s.QueryMeanMs-3.169735) > 1e-9 {
		t.Errorf("acquire/create/query mean = %v/%v/%v, want 0.030265/3.2/3.169735", s.AcquireMeanMs, s.CreateMeanMs, s.QueryMeanMs)
	}
	if s.QueryMeanMs <= 0 || s.EmptyAcquire != 50 {
		t.Errorf("query_mean = %v (want > 0), empty = %d (want 50, kept)", s.QueryMeanMs, s.EmptyAcquire)
	}
}

// 「踢慢 client」的行數：用 log 的 time 欄位對窗口；別的 log 行不算
func TestReadSlowKickTimes(t *testing.T) {
	content := `{"time":"2026-11-06T12:00:01Z","level":"WARN","msg":"ws outbox full, kicking slow client","room":"r"}` + "\n" +
		`{"time":"2026-11-06T12:00:02Z","msg":"request","path":"/kicking"}` + "\n" +
		`{"time":"2026-11-06T12:00:09Z","level":"WARN","msg":"ws outbox full, kicking slow client","room":"r"}` + "\n"
	path := filepath.Join(t.TempDir(), "s.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := readSlowKickTimes(path)
	if err != nil || len(ts) != 2 {
		t.Fatalf("kicks = %v err = %v, want 2", ts, err)
	}
	from := time.Date(2026, 11, 6, 12, 0, 5, 0, time.UTC)
	if n := countInWindow(ts, from, from.Add(10*time.Second)); n != 1 {
		t.Errorf("kicks in window = %d, want 1", n)
	}
}

// 工具靠的 log 文字要和 server 真的 log 的一致；改了 hub.go 的字，這裡要跟著壞
func TestSlowKickTextExistsInHub(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "ws", "hub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), slowKickText) {
		t.Errorf("internal/ws/hub.go no longer logs %q: update slowKickText", slowKickText)
	}
}

// /metrics 的解析與起訖相減：只認 POST 的 messages route
func TestParsePostDurationAndSummarizeHandler(t *testing.T) {
	body := `# HELP http_request_duration_seconds Duration
http_request_duration_seconds_bucket{method="POST",path="/rooms/{public_id}/messages",le="0.005"} 3
http_request_duration_seconds_sum{method="POST",path="/rooms/{public_id}/messages"} 1.5
http_request_duration_seconds_count{method="POST",path="/rooms/{public_id}/messages"} 100
http_request_duration_seconds_sum{method="GET",path="/rooms/{public_id}/messages"} 99
http_request_duration_seconds_count{method="GET",path="/rooms/{public_id}/messages"} 99
http_request_duration_seconds_sum{method="POST",path="/login"} 77
http_request_duration_seconds_count{method="POST",path="/login"} 77
`
	snap, ok, err := parsePostDuration(strings.NewReader(body))
	if err != nil || !ok || snap.Count != 100 || snap.SumSeconds != 1.5 {
		t.Fatalf("snap = %+v ok = %v err = %v, want sum 1.5 / count 100", snap, ok, err)
	}
	if _, ok, _ := parsePostDuration(strings.NewReader("go_goroutines 5\n")); ok {
		t.Error("ok for a body without the POST route")
	}

	servers := []string{"a", "b", "c"}
	before := map[string]durationSnapshot{"a": {1, 100}, "b": {0, 0}}
	after := map[string]durationSnapshot{"a": {1.5, 200}, "b": {0.3, 100}}
	h := summarizeHandler(servers, before, after, map[string]error{"c": errors.New("down")})
	if h.Count != 200 || h.MeanMs < 3.99 || h.MeanMs > 4.01 { // (0.5 + 0.3) s / 200
		t.Errorf("combined = %d / %.3fms, want 200 / 4ms", h.Count, h.MeanMs)
	}
	if h.Servers[0].MeanMs != 5 || h.Servers[1].MeanMs != 3 || h.Servers[2].Note == "" {
		t.Errorf("servers = %+v", h.Servers)
	}
}

// 表格有印出新的欄位：machine_cores、frames 預期值、closes_after_join、handler 與分項平均
func TestPrintTableShowsNewFields(t *testing.T) {
	rep := &runReport{Scenario: "mixed", NumCPU: 8, Steps: []stepResult{{
		Status: "ran", Valid: true,
		CPU:     cpuResult{MachineCores: 3.25, MachineJudged: true},
		Frames:  framesResult{PerSec: 500, Expected: 1000, Ratio: 0.5},
		Viewers: viewersResult{ClosesAfterJoin: 4},
		Handler: handlerSummary{Count: 10, MeanMs: 1.5, Servers: []handlerTiming{{Server: "s", Count: 10, MeanMs: 1.5}}},
		Timing:  []timingSummary{{Path: "/x/a.log", AcquireMeanMs: 1, QueryMeanMs: 2, LimiterMeanMs: 0.5, PublishMeanMs: 0.7, SlowKicksInWindow: 3}},
	}}, SlowKicksLogFile: map[string]int{"/x/a.log": 9}}
	var buf strings.Builder
	printTable(&buf, rep)
	out := buf.String()
	for _, want := range []string{"machine_cores=3.25 of 8", "expected=1000 (ratio 0.50)", "closes_after_join=4", "handler POST mean", "acquire_mean 1.00ms", "query_mean ~2.00ms", "limiter Allow mean=0.50ms", "redis PUBLISH mean=0.70ms", "slow-client kicks in window=3", "whole log file (a.log, not only this run): 9"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output is missing %q:\n%s", want, out)
		}
	}
}
