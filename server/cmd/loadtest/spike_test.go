package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	resyncR1    = `{"type":"resync","payload":{"room":"r1"}}`
	resyncOther = `{"type":"resync","payload":{"room":"r2"}}`
)

// 測試用的 fetcher：sleep 不真的等，而是把「我在等」交給測試，由測試決定何時放行（不靠 time.Sleep）；
// fetch 記下呼叫並可選擇卡住
type spikeHarness struct {
	f       *resyncFetcher
	sleeps  chan chan struct{} // 每次排程延遲就送進來一個放行用的 channel
	started chan string        // 每次 GET 開始就送進來 server
	hold    chan struct{}      // 非 nil 時，fetch 會等它被關閉才回
	fetchFn func(server string) (int, error)
}

func newSpikeHarness(ctx context.Context) *spikeHarness {
	h := &spikeHarness{sleeps: make(chan chan struct{}, 16), started: make(chan string, 16)}
	h.fetchFn = func(string) (int, error) { return 200, nil }
	h.f = newResyncFetcher(ctx, time.Second, func(ctx context.Context, server, room string) (int, error) {
		h.started <- server
		if h.hold != nil {
			<-h.hold
		}
		return h.fetchFn(server)
	})
	h.f.sleepFn = func(ctx context.Context, d time.Duration) bool {
		release := make(chan struct{})
		h.sleeps <- release
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	return h
}

// 等 fetcher 的 goroutine 排進下一個延遲（它是非同步起來的）；逾時代表沒有排，測試失敗
func (h *spikeHarness) nextSleep(t *testing.T) chan struct{} {
	t.Helper()
	select {
	case r := <-h.sleeps:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no fetch was scheduled")
		return nil
	}
}

func TestResyncFetcherDedupesPending(t *testing.T) {
	h := newSpikeHarness(context.Background())
	t0 := time.Unix(1000, 0)
	h.f.OnFrame(7, "http://a", "r1", []byte(resyncR1), t0)
	h.f.OnFrame(7, "http://a", "r1", []byte(resyncR1), t0.Add(time.Millisecond)) // 還在等：忽略
	h.f.OnFrame(8, "http://a", "r1", []byte(resyncR1), t0)                       // 別的觀眾各自一份
	close(h.nextSleep(t))                                                        // 觀眾 7 與 8 各一個
	close(h.nextSleep(t))
	h.f.Wait()
	fetches, resyncs := h.f.Snapshot()
	if len(fetches) != 2 {
		t.Fatalf("fetches = %d, want 2", len(fetches))
	}
	if resyncs[7] != 2 || resyncs[8] != 1 { // 被忽略的 resync 仍然算收到
		t.Errorf("resyncs = %v", resyncs)
	}
	if len(h.sleeps) != 0 {
		t.Error("an extra fetch was scheduled")
	}
}

func TestResyncFetcherNewFetchAfterStart(t *testing.T) {
	h := newSpikeHarness(context.Background())
	h.hold = make(chan struct{})
	t0 := time.Unix(1000, 0)
	h.f.OnFrame(1, "http://a", "r1", []byte(resyncR1), t0)
	close(h.nextSleep(t))
	<-h.started // 第一次 GET 已經開始（卡在 hold）：不再算 pending
	h.f.OnFrame(1, "http://a", "r1", []byte(resyncR1), t0.Add(time.Second))
	close(h.nextSleep(t)) // 第一次 GET 開始後來的 resync 要另排一次
	<-h.started
	close(h.hold)
	h.f.Wait()
	fetches, resyncs := h.f.Snapshot()
	if len(fetches) != 2 || resyncs[1] != 2 {
		t.Errorf("fetches = %d resyncs = %v, want 2 and 2", len(fetches), resyncs)
	}
}

func TestResyncFetcherIgnoresOtherFramesAndRooms(t *testing.T) {
	h := newSpikeHarness(context.Background())
	for _, msg := range []string{
		resyncOther, // 別的房間
		`{"type":"message","payload":{"room":"r1","content":"x"}}`,
		`{"type":"presence","payload":{"room":"r1","count":3}}`,
		`{"type":"message","payload":{"content":"{\"type\":\"resync\"}"}}`, // 內容含關鍵字，但不是 resync frame
		`not json "type":"resync"`,
	} {
		h.f.OnFrame(1, "http://a", "r1", []byte(msg), time.Unix(1000, 0))
	}
	h.f.Wait()
	fetches, resyncs := h.f.Snapshot()
	if len(h.sleeps) != 0 || len(fetches) != 0 || len(resyncs) != 0 {
		t.Errorf("sleeps=%d fetches=%d resyncs=%v, want none", len(h.sleeps), len(fetches), resyncs)
	}
}

func TestResyncFetcherRecordsResult(t *testing.T) {
	h := newSpikeHarness(context.Background())
	var calls int
	h.fetchFn = func(string) (int, error) {
		calls++
		if calls == 1 {
			return 500, nil
		}
		return 0, &url.Error{Op: "Get", URL: "http://a/rooms/r1/messages", Err: errors.New("connection refused")}
	}
	base := time.Unix(2000, 0)
	var n int
	h.f.nowFn = func() time.Time { n++; return base.Add(time.Duration(n) * 10 * time.Millisecond) } // start、end 各呼叫一次：每次 GET 耗 10ms
	sched := time.Unix(1999, 0)
	h.f.OnFrame(1, "http://a", "r1", []byte(resyncR1), sched)
	close(h.nextSleep(t))
	h.f.Wait()
	h.f.OnFrame(1, "http://a", "r1", []byte(resyncR1), sched)
	close(h.nextSleep(t))
	h.f.Wait()
	fetches, _ := h.f.Snapshot()
	if len(fetches) != 2 {
		t.Fatalf("fetches = %d", len(fetches))
	}
	a, b := fetches[0], fetches[1]
	if a.Status != 500 || a.Err != "" || a.ok() || a.failKey() != "500" || a.Duration != 10*time.Millisecond || !a.Scheduled.Equal(sched) || a.Viewer != 1 || a.Server != "http://a" || a.Room != "r1" {
		t.Errorf("first = %+v", a)
	}
	if b.Status != 0 || b.Err != "connection refused" || b.ok() || b.failKey() != "err: connection refused" { // url.Error 的 URL 要被拆掉
		t.Errorf("second = %+v", b)
	}
	if !b.Start.After(a.Start) {
		t.Error("Snapshot should be ordered by start")
	}
}

func TestResyncFetcherCancelDropsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newSpikeHarness(ctx)
	h.f.OnFrame(1, "http://a", "r1", []byte(resyncR1), time.Unix(1000, 0))
	h.nextSleep(t)
	cancel()
	h.f.Wait()
	if fetches, _ := h.f.Snapshot(); len(fetches) != 0 {
		t.Errorf("a cancelled wait must not fetch: %v", fetches)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if h.f.pending[1] {
		t.Error("pending should be cleared after cancel")
	}
}

// 真的走 HTTP（沒注入 fetchFn）：delayMax = 0 不等，status / 傳輸錯誤要分得出來
func TestHTTPResyncFetchStatuses(t *testing.T) {
	var status = http.StatusOK
	body := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rooms/r1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	fetch := httpResyncFetch(srv.Client(), "run")
	if st, err := fetch(context.Background(), srv.URL, "r1"); st != 200 || err != nil {
		t.Errorf("ok: %d %v", st, err)
	}
	status = http.StatusInternalServerError
	if st, err := fetch(context.Background(), srv.URL, "r1"); st != 500 || err != nil {
		t.Errorf("500: %d %v", st, err)
	}
	status, body = http.StatusOK, `not json`
	if st, err := fetch(context.Background(), srv.URL, "r1"); st != 200 || !errors.Is(err, errDecodeHistory) {
		t.Errorf("bad body: %d %v", st, err)
	}
	srv.Close()
	if st, err := fetch(context.Background(), srv.URL, "r1"); st != 0 || err == nil {
		t.Errorf("closed server: %d %v", st, err)
	}
}

func TestSummarizeSpikeLatency(t *testing.T) {
	if got := summarizeSpikeLatency(nil); got != (spikeLatency{}) {
		t.Errorf("empty = %+v", got)
	}
	var d []time.Duration
	for i := 10; i >= 1; i-- { // 10..1 ms，故意倒序
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	got := summarizeSpikeLatency(d)
	// nearest-rank：p50 = 第 5 筆、p90 = 第 9 筆、p99 = 第 10 筆
	if got.N != 10 || got.P50Ms != 5 || got.P90Ms != 9 || got.P99Ms != 10 || got.MaxMs != 10 {
		t.Errorf("got %+v", got)
	}
	if d[0] != 10*time.Millisecond {
		t.Error("input must not be reordered")
	}
}

func TestBucketStartsAndPeaks(t *testing.T) {
	if bucketStarts(nil, spikeBucket) != nil || peakWindow(nil, 10) != 0 {
		t.Error("empty input")
	}
	t0 := time.Unix(1000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	// 無序輸入：最早是 at(50)，所以桶界在 50 / 150 / 250 ...
	got := bucketStarts([]time.Time{at(260), at(50), at(149), at(150), at(151), at(60)}, spikeBucket)
	want := []int{3, 2, 1} // [50,150): 50 60 149；[150,250): 150 151；[250,350): 260
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("buckets = %v, want %v", got, want)
	}
	if peakWindow(got, 1) != 3 || peakWindow(got, 2) != 5 || peakWindow(got, 10) != 6 {
		t.Errorf("peaks = %d %d %d", peakWindow(got, 1), peakWindow(got, 2), peakWindow(got, 10))
	}
	// 滑動窗口：長度 12 的序列，最大的 10 連續格要跨過中間
	seq := []int{0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0, 7}
	if p := peakWindow(seq, 10); p != 12 { // 後 10 格 [2..11] 含 5 與 7
		t.Errorf("sliding peak = %d, want 12", p)
	}
}

func TestCountDistribution(t *testing.T) {
	got := countDistribution([]int{0, 1, 2, 3}, map[int]int{0: 1, 1: 2, 2: 2, 99: 5}) // 3 沒出現：0 次；99 不是觀眾：不算
	if len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 2 {
		t.Errorf("dist = %v", got)
	}
}

func TestBuildSpikeReport(t *testing.T) {
	start := time.Unix(5000, 0)
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	ms := time.Millisecond
	fetches := []resyncFetch{
		{Viewer: 0, Server: "http://a", Start: at(30000), Duration: 10 * ms, Status: 200},
		{Viewer: 1, Server: "http://a", Start: at(30050), Duration: 30 * ms, Status: 200},
		{Viewer: 1, Server: "http://a", Start: at(30120), Duration: 5 * ms, Status: 500},
		{Viewer: 2, Server: "http://b", Start: at(31000), Duration: 7 * ms, Err: "timeout"},
		{Viewer: 2, Server: "http://b", Start: at(31001), Duration: 20 * ms, Status: 200},
	}
	samples := []redisMetricSample{
		{Server: "http://a", ResyncSent: 1, PublishFailed: 2}, {Server: "http://b", ResyncSent: 0, PublishFailed: 0},
		{Server: "http://a", ResyncSent: 11, PublishFailed: 9}, {Server: "http://b", ResyncSent: 4, PublishFailed: 1},
	}
	rep := buildSpikeReport(spikeInput{
		Start: start, Config: spikeConfigJSON{Rooms: 1, ViewersPerRoom: 4, ViewersTotal: 4},
		Viewers: map[int]string{0: "http://a", 1: "http://a", 2: "http://b", 3: "http://b"}, // 3 號一個 resync 都沒收到
		Servers: []string{"http://a", "http://b"}, Fetches: fetches, Resyncs: map[int]int{0: 1, 1: 3, 2: 2},
		Posts: map[string]*redisPostStats{"http://a": {Sent: 9}}, Samples: samples, SampErr: map[string]int{"http://b": 2},
	})
	g := rep.Gets
	if g.Total != 5 || g.OK != 3 || g.Failed != 2 || g.FailedBy["500"] != 1 || g.FailedBy["err: timeout"] != 1 {
		t.Errorf("gets = %+v", g)
	}
	if g.Latency.N != 3 || g.Latency.P50Ms != 20 || g.Latency.MaxMs != 30 { // 失敗的 5ms / 7ms 不算
		t.Errorf("latency = %+v", g.Latency)
	}
	tl := rep.Timeline
	if tl.FirstGetStartS == nil || *tl.FirstGetStartS != 30 || *tl.LastGetStartS != 31.001 {
		t.Errorf("first/last = %v %v", tl.FirstGetStartS, tl.LastGetStartS)
	}
	if len(tl.Per100ms) != 11 || tl.Per100ms[0] != 2 || tl.Per100ms[1] != 1 || tl.Per100ms[10] != 2 || tl.PeakPer100ms != 2 || tl.PeakPer1s != 3 {
		t.Errorf("timeline = %+v", tl)
	}
	// resync：0 → 1 條、1 → 1 條（viewer 3）… 0 次的是 viewer 3；1 次 = viewer 0；2 次 = viewer 2；3 次 = viewer 1
	rs := rep.PerViewer.ResyncFrames
	if len(rs) != 4 || rs[0] != 1 || rs[1] != 1 || rs[2] != 1 || rs[3] != 1 {
		t.Errorf("resync dist = %v", rs)
	}
	gs := rep.PerViewer.Gets // viewer 0:1 次、1:2 次、2:2 次、3:0 次
	if len(gs) != 3 || gs[0] != 1 || gs[1] != 1 || gs[2] != 2 {
		t.Errorf("get dist = %v", gs)
	}
	a, b := rep.Servers[0], rep.Servers[1]
	if a.Viewers != 2 || a.ResyncSentDelta != 10 || a.PublishFailedDelta != 7 || a.MetricSamplesOK != 2 || a.Posts == nil || a.Posts.Sent != 9 {
		t.Errorf("a = %+v", a)
	}
	if b.ResyncSentDelta != 4 || b.PublishFailedDelta != 1 || b.MetricSampleErrors != 2 || b.Posts != nil {
		t.Errorf("b = %+v", b)
	}
	if rep.Note == "" {
		t.Error("note is empty")
	}
	var sb strings.Builder
	printSpike(&sb, &rep)
	for _, want := range []string{"gets: total=5 ok=3 failed=2", "peak=2/100ms 3/1s", "resync frames -> viewers: 0:1 1:1 2:1 3:1", "http://a: viewers=2 metrics: resync_sent+10 publish_failed+7"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("print output lacks %q:\n%s", want, sb.String())
		}
	}
	// 沒有任何 GET：不 panic，時間軸欄位為空
	empty := buildSpikeReport(spikeInput{Start: start, Viewers: map[int]string{0: "http://a"}, Servers: []string{"http://a"}})
	if empty.Timeline.FirstGetStartS != nil || empty.Gets.Total != 0 || empty.PerViewer.Gets[0] != 1 {
		t.Errorf("empty = %+v", empty)
	}
	printSpike(&sb, &empty)
}

func TestBuildPostStatsByServerRooms(t *testing.T) {
	// 2 個房間、2 台 server：server = (seq / 2) % 2 → seq 0,1 → a；2,3 → b
	res := &openLoopResult{
		Interval: time.Second, Total: 4,
		Outcome:   []int8{outcomeResponse, outcomeResponse, outcomeResponse, outcomeResponse},
		Status:    []int32{201, 201, 500, 500},
		LatencyNs: []int64{1, 1, 1, 1},
	}
	got := buildPostStatsByServerRooms(res, []string{"http://a", "http://b"}, 2)
	if got["http://a"].StatusCounts["201"] != 2 || got["http://b"].StatusCounts["500"] != 2 {
		t.Errorf("got a=%+v b=%+v", got["http://a"], got["http://b"])
	}
}

func TestSpikeFlagsAndValidation(t *testing.T) {
	o, err := parseFlags([]string{"-scenario", "resync-spike"})
	if err != nil || o.duration != 90*time.Second || o.spikeRooms != 100 || o.spikeViewersPerRoom != 10 || o.spikeRoomRate != 1 || o.redisResyncDelayMax != 2*time.Second {
		t.Errorf("defaults: %+v err=%v", o, err)
	}
	if o, _ := parseFlags([]string{"-scenario", "resync-spike", "-duration", "40s"}); o.duration != 40*time.Second {
		t.Errorf("explicit duration overridden: %v", o.duration)
	}
	if o, _ := parseFlags([]string{"-scenario", "redis"}); o.duration != 120*time.Second {
		t.Errorf("redis default duration changed: %v", o.duration)
	}
	steps, err := buildSteps(&options{scenario: "resync-spike", spikeRooms: 100, spikeViewersPerRoom: 10, spikeRoomRate: 2, redisResyncDelayMax: 2 * time.Second})
	if err != nil || len(steps) != 1 || steps[0].Rooms != 100 || steps[0].Viewers != 1000 || steps[0].Rate != 200 {
		t.Errorf("steps = %+v, %v", steps, err)
	}
	if _, err := buildSteps(&options{scenario: "resync-spike", spikeRooms: 1, spikeViewersPerRoom: 1, spikeRoomRate: 1, redisResyncDelayMax: 0}); err != nil {
		t.Errorf("delay-max 0 should be accepted: %v", err)
	}
	for _, bad := range []options{
		{spikeRooms: 0, spikeViewersPerRoom: 1, spikeRoomRate: 1},
		{spikeRooms: 1, spikeViewersPerRoom: 0, spikeRoomRate: 1},
		{spikeRooms: 1, spikeViewersPerRoom: 1, spikeRoomRate: 0},
		{spikeRooms: 1, spikeViewersPerRoom: 1, spikeRoomRate: -1},
		{spikeRooms: 1, spikeViewersPerRoom: 1, spikeRoomRate: 1, redisResyncDelayMax: -time.Second},
	} {
		bad.scenario = "resync-spike"
		if _, err := buildSteps(&bad); err == nil {
			t.Errorf("buildSteps(%+v) = nil error", bad)
		}
	}
}

// 斷線只記第一次、依 server 彙整；沒斷線時是 0
func TestSpikeDisconnects(t *testing.T) {
	d := &spikeDisconnects{}
	if len(d.snapshot()) != 0 {
		t.Fatal("expected empty snapshot")
	}
	d.onClose(&viewer{idx: 1, server: "http://a"}, errors.New("x"), time.Now())
	d.onClose(&viewer{idx: 1, server: "http://a"}, errors.New("y"), time.Now())
	d.onClose(&viewer{idx: 2, server: "http://b"}, errors.New("x"), time.Now())
	snap := d.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot = %v, want 2 viewers", snap)
	}
	rep := buildSpikeReport(spikeInput{
		Start: time.Unix(5000, 0), Servers: []string{"http://a", "http://b", "http://c"},
		Viewers: map[int]string{1: "http://a", 2: "http://b"}, Disconnects: snap,
	})
	if rep.Disconnects != 2 || rep.Servers[0].Disconnects != 1 || rep.Servers[1].Disconnects != 1 || rep.Servers[2].Disconnects != 0 {
		t.Fatalf("disconnects = %d, servers = %+v", rep.Disconnects, rep.Servers)
	}
}
