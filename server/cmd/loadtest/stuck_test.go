package main

import (
	"strings"
	"testing"
	"time"
)

const stuckMetricsText = `# HELP go_threads Number of OS threads created.
# TYPE go_threads gauge
go_threads 14
# HELP process_resident_memory_bytes Resident memory size in bytes.
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 3.2e+07
# TYPE websocket_connections_active gauge
websocket_connections_active 7
http_request_duration_seconds_count{method="GET",path="/x"} 3
`

func TestParseStuckMetrics(t *testing.T) {
	m, err := parseStuckMetrics(strings.NewReader(stuckMetricsText))
	if err != nil {
		t.Fatal(err)
	}
	if m.Connections != 7 || m.GoThreads != 14 || m.RSSBytes != 3.2e7 {
		t.Errorf("got %+v", m)
	}

	// 缺值：回 error 並列出缺的名稱，不當成 0
	_, err = parseStuckMetrics(strings.NewReader("go_threads 5\n"))
	if err == nil || !strings.Contains(err.Error(), metricWSConnections) || !strings.Contains(err.Error(), metricRSSBytes) || strings.Contains(err.Error(), metricGoThreads) {
		t.Errorf("missing metrics error = %v", err)
	}
	if _, err := parseStuckMetrics(strings.NewReader("")); err == nil {
		t.Error("empty body should be an error")
	}
	// 值不是數字
	if _, err := parseStuckMetrics(strings.NewReader("go_threads abc\n")); err == nil {
		t.Error("non-numeric value should be an error")
	}
}

// 每秒一筆的 sample：t0 是 ramp 開始，connect 在 t0+1s，全部連上在 t0+3s
func stuckTestInput(rows [][3]int) stuckInput { // 每列 {connections, threads, joined}
	t0 := time.Unix(1000, 0)
	in := stuckInput{
		Stuck: 3, Baseline: stuckMetrics{Connections: 0, GoThreads: 10, RSSBytes: 1e7},
		RampStart: t0, FirstConnect: t0.Add(1 * time.Second), AllConnected: t0.Add(3 * time.Second), Joined: 3,
	}
	for i, r := range rows {
		in.Samples = append(in.Samples, stuckSample{Connections: r[0], GoThreads: r[1], Joined: r[2], RSSBytes: 2e7, at: t0.Add(time.Duration(i) * time.Second)})
	}
	return in
}

func TestAnalyzeStuckKicks(t *testing.T) {
	// 第 4 秒（index 4）第一條被踢：joined 3 但 connections 2；第 6 秒回到基準
	rep := analyzeStuck(stuckTestInput([][3]int{
		{0, 10, 0}, {1, 11, 1}, {2, 12, 2}, {3, 15, 3}, {2, 16, 3}, {1, 16, 3}, {0, 16, 3}, {0, 14, 3},
	}))
	s := rep.Summary
	if s.FirstKick == nil || s.FirstKick.SinceFirstConnectS != 3 || s.FirstKick.SinceAllConnectedS != 1 {
		t.Errorf("first kick = %+v, want 3s after first connect / 1s after all connected", s.FirstKick)
	}
	if s.AllKicked == nil || s.AllKicked.SinceFirstConnectS != 5 || s.AllKicked.SinceAllConnectedS != 3 {
		t.Errorf("all kicked = %+v, want 5s after first connect / 3s after all connected", s.AllKicked)
	}
	if s.MaxGoThreads != 16 || s.MaxThreadsAt.SinceFirstConnectS != 3 {
		t.Errorf("max threads = %d at %+v, want 16 at the first sample that reached it", s.MaxGoThreads, s.MaxThreadsAt)
	}
	if s.OpenAtEnd != 0 || s.BaselineGoThreads != 10 {
		t.Errorf("summary = %+v", s)
	}
	if got := rep.Samples[0].SinceFirstConnectS; got != -1 {
		t.Errorf("sample before the first connect has t = %v, want -1", got)
	}
}

// ramp 還在進行時就有人被踢：connections 仍在增加，但低於「工具端 joined」，要被判成第一條被踢
func TestAnalyzeStuckKickDuringRamp(t *testing.T) {
	rep := analyzeStuck(stuckTestInput([][3]int{
		{0, 10, 0}, {1, 11, 1}, {2, 12, 2}, {2, 12, 3}, {3, 12, 3},
	}))
	// index 3：joined 3、connections 2 → 差 1
	if k := rep.Summary.FirstKick; k == nil || k.SinceFirstConnectS != 2 {
		t.Errorf("first kick = %+v, want 2s after first connect", k)
	}
	if rep.Summary.AllKicked != nil {
		t.Errorf("all kicked = %+v, want nil (connections never returned to baseline after ramp)", rep.Summary.AllKicked)
	}
}

// 都沒被踢：兩個時間點都是 nil，結束時仍開著 = 最後一筆 - 基準；基準不為 0 時也要扣掉
func TestAnalyzeStuckNoKick(t *testing.T) {
	in := stuckTestInput([][3]int{{2, 10, 0}, {3, 10, 1}, {5, 10, 3}, {5, 10, 3}})
	in.Baseline.Connections = 2
	rep := analyzeStuck(in)
	if rep.Summary.FirstKick != nil || rep.Summary.AllKicked != nil {
		t.Errorf("kicks = %+v / %+v, want none", rep.Summary.FirstKick, rep.Summary.AllKicked)
	}
	if rep.Summary.OpenAtEnd != 3 {
		t.Errorf("open at end = %d, want 3", rep.Summary.OpenAtEnd)
	}
}

// stuck 沒明確給 -duration 時預設 90s；明確給了就照給的；其他 scenario 維持 30s
func TestStuckDefaultDuration(t *testing.T) {
	cases := []struct {
		args []string
		want time.Duration
	}{
		{[]string{"-scenario", "stuck"}, 90 * time.Second},
		{[]string{"-scenario", "stuck", "-duration", "30s"}, 30 * time.Second},
		{[]string{"-scenario", "stuck", "-duration", "120s"}, 120 * time.Second},
		{[]string{"-scenario", "mixed"}, 30 * time.Second},
	}
	for _, c := range cases {
		o, err := parseFlags(c.args)
		if err != nil || o.duration != c.want {
			t.Errorf("parseFlags(%v) duration = %v, %v; want %v", c.args, o.duration, err, c.want)
		}
	}
}
