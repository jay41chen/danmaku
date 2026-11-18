package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 跟 client/src/lib/backoff.ts 的 equalJitter 逐項對：base = min(cap, 1000 * 2^attempt)ms，wait = base/2 + r * base/2
func TestEqualJitterMatchesClient(t *testing.T) {
	for _, c := range []struct {
		attempt int
		r       float64
		wantMs  float64
	}{
		{0, 0, 500}, {0, 0.5, 750}, {0, 1, 1000},
		{1, 0, 1000}, {1, 0.5, 1500},
		{3, 0.25, 4000 + 0.25*4000}, // base 8000
		{5, 0, 16000},               // base 32000（剛好封頂）
		{6, 1, 32000},               // 64000 → 封頂 32000
		{40, 0.5, 24000},            // 2^40 不溢位，仍是封頂值
	} {
		got := equalJitter(c.attempt, time.Second, 32*time.Second, func() float64 { return c.r })
		if math.Abs(durMs(got)-c.wantMs) > 0.001 {
			t.Errorf("attempt %d r=%v: got %v ms, want %v ms", c.attempt, c.r, durMs(got), c.wantMs)
		}
	}
}

func TestParsePresenceFrame(t *testing.T) {
	for _, c := range []struct {
		msg   string
		count int
		ok    bool
	}{
		{`{"type":"presence","payload":{"room":"r1","count":7}}`, 7, true},
		{`{"type":"presence","payload":{"room":"r1","count":0}}`, 0, true},
		{`{"type":"presence","payload":{"room":"other","count":7}}`, 0, false},
		{`{"type":"presence","payload":{"room":"r1"}}`, 0, false},
		{`{"type":"message","payload":{"content":"\"type\":\"presence\""}}`, 0, false},
		{`not json`, 0, false},
	} {
		n, ok := parsePresenceFrame("r1", []byte(c.msg))
		if n != c.count || ok != c.ok {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.msg, n, ok, c.count, c.ok)
		}
	}
}

func TestComputeDowntimes(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	joins := []time.Time{at(1), at(40), at(90)}
	disc := []time.Time{at(30), at(80), at(100)}
	got, pending := computeDowntimes(joins, disc)
	if !slices.Equal(got, []time.Duration{10 * time.Second, 10 * time.Second}) || pending != 1 {
		t.Errorf("downtimes = %v pending = %d", got, pending)
	}
	spans := joinedSpans(joins, disc)
	if len(spans) != 3 || !spans[0].To.Equal(at(30)) || !spans[2].To.Equal(at(100)) {
		t.Errorf("spans = %+v", spans)
	}
	// 沒斷線：區間開放
	if sp := joinedSpans([]time.Time{at(1)}, nil); len(sp) != 1 || !sp[0].To.IsZero() {
		t.Errorf("open span = %+v", sp)
	}
}

func TestExpectedJoined(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	spans := [][]joinedSpan{
		{{From: at(1), To: at(30)}, {From: at(40)}}, // 30~40 斷線
		{{From: at(2)}},
		{{From: at(50)}}, // 50 才進來
	}
	for _, c := range []struct{ s, want int }{{0, 0}, {1, 1}, {10, 2}, {35, 1}, {45, 2}, {60, 3}} {
		if got := expectedJoined(spans, at(c.s)); got != c.want {
			t.Errorf("expected at %ds = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestSummarizePresence(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	expected := func(t time.Time) int { // 10 人，kill 在 30s 掉到 5 人
		if t.Before(at(30)) {
			return 10
		}
		return 5
	}
	frames := []presenceFrame{
		{at(5), 10}, {at(20), 10},
		{at(35), 10}, // 匿名 stale 還沒剔除：偏高
		{at(40), 8},
		{at(50), 5},
		{at(55), 5},
	}
	s := summarizePresence(frames, expected, at(30), t0)
	if s.Frames != 6 || s.FramesDifferedTotal != 2 {
		t.Errorf("frames = %d differed = %d", s.Frames, s.FramesDifferedTotal)
	}
	if s.MaxAfterKill == nil || *s.MaxAfterKill != 10 || *s.ExpectedAtMaxAfter != 5 || *s.MaxExcessAfterKill != 5 || *s.MinDeficitAfterKill != 0 {
		t.Errorf("after kill = %+v", s)
	}
	if s.LastDiffAtS == nil || *s.LastDiffAtS != 40 || *s.LastDiffAfterKillS != 10 {
		t.Errorf("last diff = %v / %v", s.LastDiffAtS, s.LastDiffAfterKillS)
	}
	// 沒有斷線、沒有不同：指標都是 nil
	s = summarizePresence(frames[:2], expected, time.Time{}, t0)
	if s.MaxAfterKill != nil || s.LastDiffAtS != nil {
		t.Errorf("no kill: %+v", s)
	}
}

// kill 報告沿用 redis 情境的 computeOutcome：空窗期間漏收、join_ok 後撈歷史撈回來的算 recovered
func TestBuildKillReportReusesOutcome(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	created := []int{0, 1, 2, 3, 4}
	ws := map[int]time.Time{0: at(1), 1: at(2), 4: at(20)}
	recovered := map[int]struct{}{2: {}} // 3 沒撈到
	o := computeOutcome(created, ws, recovered)
	if !slices.Equal(o.Missing, []int{2, 3}) || !slices.Equal(o.Recovered, []int{2}) || !slices.Equal(o.FinalMissing, []int{3}) {
		t.Fatalf("outcome = %+v", o)
	}

	base := redisReport{
		Summary: redisSummary{PostStatus: map[string]int{"201": 5, "502": 2}, ReceivedNot201: 1},
		Viewers: []redisViewerResult{
			{Viewer: 0, Missing: 2, Recovered: 1, FinalMissing: 1},
			{Viewer: 1},
		},
	}
	ks := []*killViewerState{
		{idx: 1, loggedIn: false},
		{idx: 0, loggedIn: true, joins: []time.Time{at(1), at(14)}, disconnects: []time.Time{at(10)}, attempts: 3,
			presence: []presenceFrame{{at(5), 2}, {at(16), 2}}},
	}
	rep := buildKillReport(t0, base, ks)
	s := rep.Summary
	if s.Viewers != 2 || s.LoggedIn != 1 || s.Anonymous != 1 || s.Disconnected != 1 || s.DisconnectedLogged != 1 || s.DisconnectedAnon != 0 {
		t.Errorf("counts = %+v", s)
	}
	if s.DowntimeMinS == nil || *s.DowntimeMinS != 4 || *s.DowntimeMedianS != 4 || *s.DowntimeMaxS != 4 || s.ReconnectAttempts != 3 {
		t.Errorf("downtime = %v %v %v attempts=%d", s.DowntimeMinS, s.DowntimeMedianS, s.DowntimeMaxS, s.ReconnectAttempts)
	}
	if s.Missing != 2 || s.Recovered != 1 || s.FinalMissing != 1 || s.Post502 != 2 || s.ReceivedNot201 != 1 {
		t.Errorf("history/post = %+v", s)
	}
	if rep.Viewers[0].Viewer != 0 || len(rep.Viewers[0].Presence) != 2 {
		t.Errorf("viewers = %+v", rep.Viewers)
	}
}

func TestKillFlagDefaults(t *testing.T) {
	o, err := parseFlags([]string{"-scenario", "kill"})
	if err != nil || o.duration != 120*time.Second || o.killViewers != 10 || o.killRate != 5 || o.killEntry != "http://nginx:80" {
		t.Errorf("defaults: %+v err=%v", o, err)
	}
	if o, _ := parseFlags([]string{"-scenario", "kill", "-duration", "40s"}); o.duration != 40*time.Second {
		t.Errorf("explicit duration overridden: %v", o.duration)
	}
	if _, err := checkServerURL("http://nginx:80", true); err != nil {
		t.Errorf("entry should pass the in-compose allowlist: %v", err)
	}
}

func TestKillLoginPattern(t *testing.T) {
	var got []bool
	for i := range 8 {
		got = append(got, killLoggedIn(i))
	}
	want := []bool{true, true, false, false, true, true, false, false}
	if !slices.Equal(got, want) {
		t.Errorf("pattern = %v, want %v", got, want)
	}
	for n, want := range map[int]int{0: 0, 1: 1, 2: 2, 3: 2, 4: 2, 5: 3, 6: 4, 7: 4, 10: 6} {
		c := 0
		for i := range n {
			if killLoggedIn(i) {
				c++
			}
		}
		if killLoggedInCount(n) != c || c != want {
			t.Errorf("n=%d: count=%d loop=%d want=%d", n, killLoggedInCount(n), c, want)
		}
	}
	// token index 要對每個登入觀眾唯一、連續
	next := 0
	for i := range 12 {
		if !killLoggedIn(i) {
			continue
		}
		if killTokenIndex(i) != next {
			t.Errorf("idx %d: token index %d, want %d", i, killTokenIndex(i), next)
		}
		next++
	}
}

func TestHistoryRetryable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500", &historyStatusError{Status: 500}, true},
		{"502", &historyStatusError{Status: 502}, true},
		{"404", &historyStatusError{Status: 404}, false},
		{"429", &historyStatusError{Status: 429}, false},
		{"network", errors.New("dial tcp: connection refused"), true},
		{"timeout", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"decode", fmt.Errorf("%w: bad", errDecodeHistory), false},
	} {
		if got := historyRetryable(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// 用真的 httptest server 走 fetchHistorySeqs：前 failFirst 次回 status，之後回 200。回傳 (kill 觀眾狀態, GET 次數, 最後結果)
func runHistoryRetry(t *testing.T, status, failFirst int) (*killViewerState, int, error) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= failFirst {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	obs := &redisObserver{ctx: context.Background(), client: srv.Client(), run: "r", room: "room", states: map[int]*redisViewerState{}}
	kr := &killRun{obs: obs, entry: srv.URL, room: "room", historyBackoff: func(int) time.Duration { return time.Millisecond }}
	ks := &killViewerState{idx: 0, v: &viewer{idx: 0, server: srv.URL}}
	_, err := kr.fetchHistory(ks, ks.nextHistoryGen())
	return ks, int(calls.Load()), err
}

func TestFetchHistoryRetryPolicy(t *testing.T) {
	// 5xx 一次後成功：2 次
	ks, calls, err := runHistoryRetry(t, 503, 1)
	if err != nil || calls != 2 || ks.historyAttempts != 2 || ks.historyFailed != 1 || ks.historyGaveUp != 0 {
		t.Errorf("503 then ok: err=%v calls=%d state=%+v", err, calls, ks)
	}
	// 一直 5xx：3 次後放棄
	ks, calls, err = runHistoryRetry(t, 500, 100)
	if err == nil || calls != 3 || ks.historyAttempts != 3 || ks.historyFailed != 3 || ks.historyGaveUp != 1 {
		t.Errorf("always 500: err=%v calls=%d state=%+v", err, calls, ks)
	}
	// 4xx 不重試
	ks, calls, err = runHistoryRetry(t, 404, 100)
	if err == nil || calls != 1 || ks.historyAttempts != 1 || ks.historyFailed != 1 || ks.historyGaveUp != 1 {
		t.Errorf("404: err=%v calls=%d state=%+v", err, calls, ks)
	}
	// 第一次就成功：不重試
	ks, calls, err = runHistoryRetry(t, 500, 0)
	if err != nil || calls != 1 || ks.historyFailed != 0 {
		t.Errorf("ok: err=%v calls=%d state=%+v", err, calls, ks)
	}
}

// run 的 ctx 已取消：失敗不算 gave_up
func TestFetchHistoryCanceledCtxNotGaveUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	obs := &redisObserver{ctx: ctx, client: srv.Client(), run: "r", room: "room", states: map[int]*redisViewerState{}}
	kr := &killRun{obs: obs, entry: srv.URL, room: "room", historyBackoff: func(int) time.Duration { return time.Millisecond }}
	ks := &killViewerState{v: &viewer{server: srv.URL}}
	if _, err := kr.fetchHistory(ks, ks.nextHistoryGen()); err == nil || ks.historyGaveUp != 0 {
		t.Errorf("canceled ctx: err=%v state=%+v", err, ks)
	}
}

// 被較新一輪取代的重試鏈：不再發任何嘗試，也不算 gave_up
func TestFetchHistorySupersededStopsRetrying(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	obs := &redisObserver{ctx: context.Background(), client: srv.Client(), run: "r", room: "room", states: map[int]*redisViewerState{}}
	ks := &killViewerState{v: &viewer{server: srv.URL}}
	old := ks.nextHistoryGen()
	// 第一次失敗後的退避期間，新一輪開始
	kr := &killRun{obs: obs, entry: srv.URL, room: "room", historyBackoff: func(int) time.Duration { ks.nextHistoryGen(); return time.Millisecond }}
	if _, err := kr.fetchHistory(ks, old); err == nil {
		t.Errorf("want last error")
	}
	if calls.Load() != 1 || ks.historyAttempts != 1 || ks.historyGaveUp != 0 {
		t.Errorf("superseded: calls=%d state=%+v", calls.Load(), ks)
	}
}

func TestFetchHistoryRetriesNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 連不上：connection refused
	obs := &redisObserver{ctx: context.Background(), client: &http.Client{Timeout: time.Second}, run: "r", room: "room", states: map[int]*redisViewerState{}}
	kr := &killRun{obs: obs, entry: url, room: "room", historyBackoff: func(int) time.Duration { return time.Millisecond }}
	ks := &killViewerState{v: &viewer{server: url}}
	if _, err := kr.fetchHistory(ks, ks.nextHistoryGen()); err == nil || ks.historyAttempts != 3 || ks.historyGaveUp != 1 {
		t.Errorf("network error: err=%v state=%+v", err, ks)
	}
}

func TestKillNAFormatting(t *testing.T) {
	n := 0
	if fmtOptInt(nil) != "n/a" || fmtOptInt(&n) != "0" {
		t.Errorf("fmtOptInt: %q %q", fmtOptInt(nil), fmtOptInt(&n))
	}
	if fmtSnap(nil) != "n/a" || fmtSnap(&wsViewersSnap{AtS: 3, Member: 2, Anonymous: 1}) != "2/1@t=3s" {
		t.Errorf("fmtSnap: %q", fmtSnap(&wsViewersSnap{AtS: 3, Member: 2, Anonymous: 1}))
	}
	// JSON：沒量測的 per-server 欄位省略，不是 0；量測到的欄位照常
	t0 := time.Unix(1000, 0)
	base := redisReport{
		Servers: []redisServerSummary{{Server: "http://s1", ConnectionsMax: 3}},
		Metrics: []redisMetricSample{
			{TS: 1, Server: "http://s1", ViewersMember: 2, ViewersAnonymous: 3, at: t0.Add(time.Second)},
			{TS: 50, Server: "http://s1", ViewersMember: 1, ViewersAnonymous: 0, at: t0.Add(50 * time.Second)},
		},
	}
	ks := []*killViewerState{{idx: 0, loggedIn: true, joins: []time.Time{t0}, disconnects: []time.Time{t0.Add(10 * time.Second)}}}
	rep := buildKillReport(t0, base, ks)
	b, err := json.Marshal(rep.Base)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Summary map[string]any   `json:"summary"`
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"viewers", "received_ws", "missing", "recovered", "final_missing", "viewers_resynced", "disconnects"} {
		if _, ok := m.Servers[0][k]; ok {
			t.Errorf("server field %q should be omitted, got %v", k, m.Servers[0][k])
		}
	}
	if _, ok := m.Summary["disconnects"]; ok {
		t.Errorf("summary.disconnects should be omitted")
	}
	if m.Servers[0]["metric_ws_connections_max"] != float64(3) {
		t.Errorf("measured field lost: %v", m.Servers[0])
	}
	sv := rep.Base.Servers[0]
	if sv.WSViewersBeforeKill == nil || sv.WSViewersBeforeKill.Anonymous != 3 || sv.WSViewersAfterKill == nil || sv.WSViewersAfterKill.Member != 1 || sv.WSViewersEnd == nil || sv.WSViewersEnd.AtS != 50 {
		t.Errorf("snapshots: %+v %+v %+v", sv.WSViewersBeforeKill, sv.WSViewersAfterKill, sv.WSViewersEnd)
	}
	var buf bytes.Buffer
	printKill(&buf, &rep)
	out := buf.String()
	if !strings.Contains(out, "viewers=n/a") || !strings.Contains(out, "disconnects=n/a") || strings.Contains(out, "disconnects=0") {
		t.Errorf("print should use n/a:\n%s", out)
	}
}

func TestParseViewersByAuth(t *testing.T) {
	body := "# HELP websocket_viewers x\n# TYPE websocket_viewers gauge\nwebsocket_viewers{auth=\"anonymous\"} 4\nwebsocket_viewers{auth=\"member\"} 2\nwebsocket_connections_active 6\n"
	got := parseViewersByAuth([]byte(body))
	if got["member"] != 2 || got["anonymous"] != 4 || len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
