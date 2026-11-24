package main

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/usecase"
)

func TestComputeOutcome(t *testing.T) {
	t0 := time.Unix(1000, 0)
	created := []int{0, 1, 2, 3, 4, 5, 6, 7}
	ws := map[int]time.Time{0: t0, 1: t0, 2: t0, 7: t0, 99: t0} // 99 不在 created：不算
	recovered := map[int]struct{}{3: {}, 4: {}, 6: {}, 2: {}, 50: {}}
	o := computeOutcome(created, ws, recovered)
	if !slices.Equal(o.Missing, []int{3, 4, 5, 6}) {
		t.Errorf("missing = %v", o.Missing)
	}
	if !slices.Equal(o.Recovered, []int{3, 4, 6}) { // 2 已經從 WS 收到、50 不是 missing，都不算 recovered
		t.Errorf("recovered = %v", o.Recovered)
	}
	if !slices.Equal(o.FinalMissing, []int{5}) {
		t.Errorf("final_missing = %v", o.FinalMissing)
	}
	if o := computeOutcome(nil, ws, recovered); len(o.Missing) != 0 {
		t.Errorf("no created posts should give nothing missing: %v", o)
	}
}

func TestSeqRanges(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want string
	}{{nil, ""}, {[]int{4}, "4"}, {[]int{1, 2, 3, 7, 9, 10}, "1-3,7,9-10"}} {
		if got := seqRanges(c.in); got != c.want {
			t.Errorf("seqRanges(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHandleFrame(t *testing.T) {
	st := newRedisViewerState(0, "http://s")
	at := time.Unix(2000, 0)
	const run, room = "abc", "room1"
	frames := []struct {
		msg    string
		resync bool
	}{
		{`{"type":"message","payload":{"id":1,"content":"lt-abc-0-5"}}`, false},
		{`{"type":"message","payload":{"id":2,"content":"lt-abc-0-5"}}`, false}, // 重複
		{`{"type":"message","payload":{"id":3,"content":"lt-zzz-0-6"}}`, false}, // 別的 run
		{`{"type":"message","payload":{"id":4,"content":"lt-abc-1-7"}}`, false}, // 別的 step
		{`{"type":"presence","payload":{"room":"room1","count":3}}`, false},
		{`{"type":"resync","payload":{"room":"other"}}`, false},
		{`not json`, false},
		{`{"type":"resync","payload":{"room":"room1"}}`, true},
	}
	for i, f := range frames {
		if got := st.handleFrame(run, room, []byte(f.msg), at.Add(time.Duration(i)*time.Second)); got != f.resync {
			t.Errorf("frame %d resync = %v, want %v", i, got, f.resync)
		}
	}
	if len(st.ws) != 1 || !st.ws[5].Equal(at) {
		t.Errorf("ws = %v (want only seq 5 at first receive)", st.ws)
	}
	if len(st.resyncs) != 1 || !st.resyncs[0].Equal(at.Add(7*time.Second)) {
		t.Errorf("resyncs = %v", st.resyncs)
	}
}

func TestParseHistorySeqs(t *testing.T) {
	body := []byte(`[{"id":1,"content":"lt-abc-0-3"},{"id":2,"content":"hello"},{"id":3,"content":"lt-abc-0-9"},{"id":4,"content":"lt-abc-2-1"},{"id":5,"content":"lt-other-0-1"}]`)
	seqs, err := parseHistorySeqs("abc", body)
	if err != nil || !slices.Equal(seqs, []int{3, 9}) {
		t.Errorf("seqs = %v, err = %v", seqs, err)
	}
	if _, err := parseHistorySeqs("abc", []byte(`{"error":"x"}`)); err == nil {
		t.Error("non-array body should be an error")
	}
}

func TestBuildPostTimeline(t *testing.T) {
	res := &openLoopResult{
		Interval: 500 * time.Millisecond, Total: 6,
		Outcome:   []int8{outcomeResponse, outcomeResponse, outcomeResponse, outcomeError, outcomeResponse, outcomeNotRun},
		Status:    []int32{201, 201, 500, 0, 201, 0},
		LatencyNs: []int64{int64(10 * time.Millisecond), int64(30 * time.Millisecond), int64(500 * time.Millisecond), int64(5 * time.Second), int64(20 * time.Millisecond), 0},
	}
	tl := buildPostTimeline(res)
	if len(tl) != 3 {
		t.Fatalf("timeline len = %d: %+v", len(tl), tl)
	}
	if tl[0].Sent != 2 || tl[0].By["201"] != 2 || tl[0].MaxMs != 30 {
		t.Errorf("second 0 = %+v", tl[0])
	}
	if tl[1].Sent != 2 || tl[1].By["500"] != 1 || tl[1].By["err"] != 1 || tl[1].MaxMs != 5000 {
		t.Errorf("second 1 = %+v", tl[1])
	}
	if tl[2].Sent != 1 || tl[2].By["201"] != 1 { // seq 5 沒執行，不算
		t.Errorf("second 2 = %+v", tl[2])
	}
	if got := createdSeqs(res); !slices.Equal(got, []int{0, 1, 4}) {
		t.Errorf("created = %v", got)
	}
}

func TestParsePlainMetrics(t *testing.T) {
	text := "# TYPE a counter\na 3\nb{x=\"1\"} 9\nc 1.5\n"
	vals, err := parsePlainMetrics(strings.NewReader(text), "a", "c")
	if err != nil || vals["a"] != 3 || vals["c"] != 1.5 {
		t.Errorf("vals = %v err = %v", vals, err)
	}
	if _, err := parsePlainMetrics(strings.NewReader(text), "a", "b"); err == nil {
		t.Error("labelled metric should count as missing")
	}
}

func TestAddFetchMinMax(t *testing.T) {
	st := newRedisViewerState(0, "http://s")
	at := time.Unix(3000, 0)
	st.addFetch(at, []int{40, 12, 99, 57}, nil)
	st.addFetch(at, nil, nil)
	f := st.fetches[0]
	if f.Count != 4 || f.MinSeq != 12 || f.MaxSeq != 99 {
		t.Errorf("fetch = %+v", f)
	}
	if f := st.fetches[1]; f.Count != 0 || f.MinSeq != 0 || f.MaxSeq != 0 {
		t.Errorf("empty fetch = %+v", f)
	}
}

func TestFinalMissingOlderThanFetch(t *testing.T) {
	at := time.Unix(3000, 0)
	st := newRedisViewerState(0, "http://s")
	st.addFetch(at, []int{3, 4}, nil)
	st.addFetch(at, []int{6, 7, 8}, nil)  // 最近一次成功：min = 6
	st.addFetch(at, nil, errTest("boom")) // 失敗的不算
	if m, ok := latestFetchMinSeq(st.fetches); !ok || m != 6 {
		t.Errorf("latestFetchMinSeq = %d, %v", m, ok)
	}
	if _, ok := latestFetchMinSeq(nil); ok {
		t.Error("no fetch should give ok=false")
	}
	if got := countOlderThan([]int{1, 2, 5, 6, 9}, 6); got != 3 {
		t.Errorf("countOlderThan = %d, want 3", got)
	}
}

func TestBuildRedisReportNewFields(t *testing.T) {
	// seq 0..5 都有排程；0,1,2,3,5 拿到 201，4 是 500
	res := &openLoopResult{
		Interval: time.Second, Total: 6,
		Outcome:   []int8{outcomeResponse, outcomeResponse, outcomeResponse, outcomeResponse, outcomeResponse, outcomeResponse},
		Status:    []int32{201, 201, 201, 201, 500, 201},
		LatencyNs: make([]int64, 6),
	}
	t0 := time.Unix(1000, 0)
	st := newRedisViewerState(0, "http://s")
	st.ws = map[int]time.Time{3: t0, 4: t0} // 4 不在 201 集合
	st.addFetch(t0, []int{2, 5}, nil)       // min = 2：missing 0,1,2,5 → recovered 2,5；final_missing 0,1 都比 2 舊
	rep := buildRedisReport(t0, 1, res, []*redisViewerState{st}, []string{"http://s"}, nil, nil)
	v := rep.Viewers[0]
	if v.ReceivedNot201 != 1 || v.FinalMissing != 2 || v.FinalMissingOlder != 2 {
		t.Errorf("viewer = %+v", v)
	}
	if len(v.Fetches) != 1 || v.Fetches[0].MinSeq == nil || *v.Fetches[0].MinSeq != 2 || v.Fetches[0].MaxSeq == nil || *v.Fetches[0].MaxSeq != 5 {
		t.Errorf("fetches = %+v", v.Fetches)
	}
	s := rep.Summary
	if s.ReceivedNot201 != 1 || s.FinalMissingOlder != 2 || s.HistoryLimit != usecase.HistoryLimit || s.HistoryLimit == 0 {
		t.Errorf("summary = %+v", s)
	}
	// 沒有成功的補撈：older 一律 0
	st2 := newRedisViewerState(1, "http://s")
	rep = buildRedisReport(t0, 1, res, []*redisViewerState{st2}, []string{"http://s"}, nil, nil)
	if rep.Viewers[0].FinalMissingOlder != 0 || rep.Viewers[0].FinalMissing == 0 {
		t.Errorf("no-fetch viewer = %+v", rep.Viewers[0])
	}
}

// seq 含 0 的補撈：MinSeq 要輸出（指向 0）；Count == 0 的補撈 MinSeq / MaxSeq 要省略（nil）
func TestBuildRedisReportFetchMinMaxJSON(t *testing.T) {
	res := &openLoopResult{
		Interval: time.Second, Total: 4,
		Outcome:   []int8{outcomeResponse, outcomeResponse, outcomeResponse, outcomeResponse},
		Status:    []int32{201, 201, 201, 201},
		LatencyNs: make([]int64, 4),
	}
	t0 := time.Unix(1000, 0)
	st := newRedisViewerState(0, "http://s")
	st.addFetch(t0, []int{0, 3}, nil)
	st.addFetch(t0, nil, nil)
	rep := buildRedisReport(t0, 1, res, []*redisViewerState{st}, []string{"http://s"}, nil, nil)
	fs := rep.Viewers[0].Fetches
	if len(fs) != 2 {
		t.Fatalf("fetches = %+v", fs)
	}
	if fs[0].MinSeq == nil || *fs[0].MinSeq != 0 || fs[0].MaxSeq == nil || *fs[0].MaxSeq != 3 {
		t.Errorf("fetch with seq 0 = %+v", fs[0])
	}
	if fs[1].Count != 0 || fs[1].MinSeq != nil || fs[1].MaxSeq != nil {
		t.Errorf("empty fetch = %+v", fs[1])
	}
}

const resyncFrame = `{"type":"resync","payload":{"room":"room1"}}`

// 測試用 observer：fetch 以計數取代，延遲固定
func newTestObserver(ctx context.Context, delayMax, delay time.Duration, calls *atomic.Int32) *redisObserver {
	return &redisObserver{
		ctx: ctx, run: "abc", room: "room1", resyncDelayMax: delayMax, states: map[int]*redisViewerState{},
		delayFn: func() time.Duration { return delay },
		fetchFn: func(context.Context, string) ([]int, error) { calls.Add(1); return nil, nil },
	}
}

func TestResyncPendingDedupe(t *testing.T) {
	var calls atomic.Int32
	o := newTestObserver(context.Background(), time.Second, 50*time.Millisecond, &calls)
	v := &viewer{idx: 0, server: "http://s"}
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.onFrame(v, []byte(resyncFrame), time.Now()) // 第一個還在等：忽略
	o.fetchWG.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", calls.Load())
	}
	// 已經開始（pending 清掉）之後的 resync 要另排一次
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.fetchWG.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", calls.Load())
	}
	if n := len(o.state(v).fetches); n != 2 {
		t.Errorf("recorded fetches = %d, want 2", n)
	}
}

func TestResyncDelayZeroFetchesImmediately(t *testing.T) {
	var calls atomic.Int32
	o := newTestObserver(context.Background(), 0, 0, &calls)
	v := &viewer{idx: 0, server: "http://s"}
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.onFrame(v, []byte(resyncFrame), time.Now()) // 延遲 0：不去重，每個 resync 各撈一次
	o.fetchWG.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", calls.Load())
	}
}

// 補撈已經開始（pending 已清）、GET 還在進行時再來的 resync 要另排一次
func TestResyncWhileFetchInFlight(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	o := newTestObserver(context.Background(), time.Second, 0, &calls)
	o.fetchFn = func(context.Context, string) ([]int, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return nil, nil
	}
	v := &viewer{idx: 0, server: "http://s"}
	o.onFrame(v, []byte(resyncFrame), time.Now())
	<-started // 第一個 GET 已在進行中，pending 此時已清掉
	o.onFrame(v, []byte(resyncFrame), time.Now())
	<-started
	close(release)
	o.fetchWG.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", calls.Load())
	}
}

// resyncDelay 的值域：[min, min+max)；max <= 0 時固定是 min
func TestResyncDelayRange(t *testing.T) {
	min, max := 2500*time.Millisecond, 2*time.Second
	for i := 0; i < 2000; i++ {
		if d := resyncDelay(min, max); d < min || d >= min+max {
			t.Fatalf("resyncDelay = %v, want in [%v, %v)", d, min, min+max)
		}
	}
	if d := resyncDelay(min, 0); d != min {
		t.Errorf("max 0: resyncDelay = %v, want %v", d, min)
	}
	if d := resyncDelay(0, 0); d != 0 {
		t.Errorf("min 0 max 0: resyncDelay = %v, want 0", d)
	}
	f := newResyncFetcher(context.Background(), min, max, nil)
	o := &redisObserver{resyncDelayMin: min, resyncDelayMax: max}
	for i := 0; i < 200; i++ {
		if d := f.nextDelay(); d < min || d >= min+max {
			t.Fatalf("fetcher nextDelay = %v", d)
		}
		if d := o.nextDelay(); d < min || d >= min+max {
			t.Fatalf("observer nextDelay = %v", d)
		}
	}
}

// 只有固定段（max = 0、min > 0）也要走去重路徑
func TestResyncMinOnlyDedupes(t *testing.T) {
	var calls atomic.Int32
	o := newTestObserver(context.Background(), 0, 50*time.Millisecond, &calls)
	o.resyncDelayMin = time.Second // delayFn 有注入，實際等待用注入值；這裡只驗證去重
	v := &viewer{idx: 0, server: "http://s"}
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.fetchWG.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", calls.Load())
	}
}

// resyncDelayMax > 0 但抽到的延遲剛好是 0：pending 仍要清掉，之後的 resync 才不會被永遠忽略
func TestResyncDrawnDelayZeroClearsPending(t *testing.T) {
	var calls atomic.Int32
	o := newTestObserver(context.Background(), time.Second, 0, &calls)
	v := &viewer{idx: 0, server: "http://s"}
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.fetchWG.Wait()
	o.onFrame(v, []byte(resyncFrame), time.Now())
	o.fetchWG.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", calls.Load())
	}
}

func TestResyncCancelAbortsPending(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	o := newTestObserver(ctx, time.Hour, time.Hour, &calls)
	o.onFrame(&viewer{idx: 0, server: "http://s"}, []byte(resyncFrame), time.Now())
	cancel()
	o.fetchWG.Wait()
	if calls.Load() != 0 {
		t.Fatalf("fetches = %d, want 0", calls.Load())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestSummarizePresenceSeries(t *testing.T) {
	if summarizePresenceSeries(nil, time.Unix(1000, 0)) != nil {
		t.Error("no frames should give nil")
	}
	start := time.Unix(1000, 0)
	at := func(sec float64) time.Time { return start.Add(time.Duration(sec * float64(time.Second))) }
	// 任意順序；-1s 落在 -5 桶（join 在 start 之前）；0..5 桶最後一筆是 t=4.5 的 3；10 桶只有 7
	frames := []presenceFrame{
		{At: at(4.5), Count: 3}, {At: at(-1), Count: 1}, {At: at(2), Count: 5}, {At: at(12), Count: 7}, {At: at(5), Count: 2},
	}
	sum := summarizePresenceSeries(frames, start)
	if sum.Frames != 5 || sum.First != 5 || sum.Last != 7 || sum.Min != 2 || sum.Max != 7 {
		t.Fatalf("summary = %+v", sum)
	}
	// first/min/max 只看 start 之後：-1s 的 1 不算
	want := []presenceBucket{{-5, 1}, {0, 3}, {5, 2}, {10, 7}}
	if !slices.Equal(sum.Series, want) {
		t.Errorf("series = %+v, want %+v", sum.Series, want)
	}
	if got := formatPresenceSeries(sum.Series); got != "-5s:1 0s:3 5s:2 10s:7" {
		t.Errorf("formatted = %q", got)
	}
}

func TestSummarizePresenceSeriesIgnoresBeforeStart(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(sec float64) time.Time { return start.Add(time.Duration(sec * float64(time.Second))) }
	sum := summarizePresenceSeries([]presenceFrame{{At: at(-3), Count: 1}, {At: at(1), Count: 8}, {At: at(2), Count: 6}}, start)
	if sum.First != 8 || sum.Min != 6 || sum.Max != 8 || sum.Last != 6 {
		t.Errorf("summary = %+v", sum)
	}
	only := summarizePresenceSeries([]presenceFrame{{At: at(-3), Count: 4}}, start)
	if only.Frames != 1 || only.First != 0 || only.Min != 0 || only.Max != 0 || only.Last != 4 || len(only.Series) != 1 {
		t.Errorf("only-before-start summary = %+v", only)
	}
}

func TestBuildPostStatsByServer(t *testing.T) {
	servers := []string{"http://a", "http://b"}
	// 1 個房間：seq 偶數 → a、奇數 → b
	res := &openLoopResult{
		Interval: time.Second, Total: 7,
		Outcome:   []int8{outcomeResponse, outcomeResponse, outcomeResponse, outcomeError, outcomeResponse, outcomeNotRun, outcomeResponse},
		Status:    []int32{201, 201, 500, 0, 201, 0, 201},
		LatencyNs: []int64{int64(10 * time.Millisecond), int64(20 * time.Millisecond), int64(900 * time.Millisecond), int64(5 * time.Second), int64(30 * time.Millisecond), 0, int64(50 * time.Millisecond)},
	}
	got := buildPostStatsByServer(res, servers)
	a, b := got["http://a"], got["http://b"]
	// a: seq 0(201,10ms) 2(500) 4(201,30ms) 6(201,50ms)
	if a.Sent != 4 || a.StatusCounts["201"] != 3 || a.StatusCounts["500"] != 1 || a.Latency201.N != 3 || a.Latency201.P50Ms != 30 || a.Latency201.MaxMs != 50 {
		t.Errorf("a = %+v", a)
	}
	// a 的 all latency：10 / 900（500）/ 30 / 50ms
	if a.LatencyAll.N != 4 || a.LatencyAll.MaxMs != 900 {
		t.Errorf("a all = %+v", a.LatencyAll)
	}
	// b: seq 1(201,20ms) 3(err，延遲不算) 5(沒執行，不算)
	if b.Sent != 2 || b.StatusCounts["201"] != 1 || b.StatusCounts["err"] != 1 || b.Latency201.N != 1 || b.Latency201.MaxMs != 20 {
		t.Errorf("b = %+v", b)
	}
	// b 的 all latency 含 err 的 5s
	if b.LatencyAll.N != 2 || b.LatencyAll.MaxMs != 5000 {
		t.Errorf("b all = %+v", b.LatencyAll)
	}
}

func TestBuildRedisReportPresencePerServer(t *testing.T) {
	t0 := time.Unix(1000, 0)
	res := &openLoopResult{Interval: time.Second}
	s1a, s1b, s2 := newRedisViewerState(0, "http://a"), newRedisViewerState(1, "http://a"), newRedisViewerState(2, "http://b")
	s1a.addPresence(t0.Add(time.Second), 4)
	s1b.addPresence(t0.Add(2*time.Second), 2)
	s2.addPresence(t0.Add(time.Second), 6)
	rep := buildRedisReport(t0, 1, res, []*redisViewerState{s1a, s1b, s2}, []string{"http://a", "http://b"}, nil, nil)
	pa, pb := rep.Servers[0].Presence, rep.Servers[1].Presence
	if pa == nil || pa.Min != 2 || pa.Max != 4 || pa.Last != 2 || pa.Frames != 2 || len(pa.Series) != 1 || pa.Series[0].Count != 2 {
		t.Errorf("a presence = %+v", pa)
	}
	if pb == nil || pb.Min != 6 || pb.Max != 6 {
		t.Errorf("b presence = %+v", pb)
	}
	if rep.Servers[0].Posts != nil {
		t.Error("buildRedisReport must not fill Posts (runRedis does)")
	}
	var sb strings.Builder
	printRedis(&sb, &rep)
	if !strings.Contains(sb.String(), "http://a: presence min=2 max=4 first=4 last=2") {
		t.Errorf("print output:\n%s", sb.String())
	}
}
