package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
)

// -scenario resync-spike：量 Redis 恢復時的「歷史 GET 尖峰」基準。
// 每個房間 -spike-viewers-per-room 條 WS 觀眾（共 -spike-rooms 個房間），整個 run 以 -spike-room-rate × 房間數 的總速率發文；
// Redis 被 deploy/fault/redis.sh 停掉時 publish 失敗，恢復後 server 對每個房間補送 resync frame，
// 每條觀眾照 web client 的做法隨機等 [0, -redis-resync-delay-max) 再 GET /rooms/{id}/messages。
// 這裡只量基準（沒有任何快取），看 GET 在時間軸上擠成什麼樣、延遲與失敗長怎樣。

// ---- 可重用元件：resyncFetcher ----

// 一次歷史 GET 的實際動作：回傳 HTTP status；傳輸失敗回 error（此時 status 沒有意義）。
// 回應 body 的內容 fetcher 不關心
type resyncFetchFunc func(ctx context.Context, server, room string) (status int, err error)

// 一次 resync 補撈的紀錄
type resyncFetch struct {
	Viewer    int
	Server    string
	Room      string
	Scheduled time.Time     // 收到 resync frame（排定補撈）的時間
	Start     time.Time     // GET 實際開始的時間
	Duration  time.Duration // GET 開始 → 回應讀完（或失敗）
	Status    int           // HTTP status；傳輸錯誤時為 0
	Err       string        // 空 = 沒有傳輸 / 解析錯誤；status 非 200 不算在這裡
}

// 這次 GET 算不算成功：status 200 且沒有錯誤
func (f resyncFetch) ok() bool { return f.Err == "" && f.Status == http.StatusOK }

// 失敗分類的 key：有錯誤用 "err: <訊息>"，否則用 HTTP status
func (f resyncFetch) failKey() string {
	if f.Err != "" {
		return "err: " + f.Err
	}
	return strconv.Itoa(f.Status)
}

// 收到 resync frame 後的補撈排程器，跟情境無關：每條觀眾一個「補撈等待中」旗標，
// 規則和 web client（client/src/realtime/connection.ts）與 redisViewerState.tryMarkPending 一致：
// 已有等待中的補撈就忽略後來的 resync；補撈一開始（GET 發出前）旗標就清掉，所以之後再來的 resync 會另排一次。
// 和 redisObserver 不同的是：delayMax = 0 時也照樣去重（延遲 0、立刻開始），不保留「不去重」的舊行為。
// 同時記錄每條觀眾收到幾個 resync frame。所有方法都可以並行呼叫
type resyncFetcher struct {
	ctx      context.Context
	delayMax time.Duration // 延遲上限：每次補撈等 [0, delayMax)；<= 0 = 不等

	// 測試用的注入點，nil 時用預設：均勻亂數延遲 / 真的等（ctx 取消回 false）/ 真的 GET（呼叫端提供）
	delayFn func() time.Duration
	sleepFn func(ctx context.Context, d time.Duration) bool
	fetchFn resyncFetchFunc
	nowFn   func() time.Time

	mu      sync.Mutex
	pending map[int]bool // viewer idx → 已排定、還沒開始的補撈
	resyncs map[int]int  // viewer idx → 收到的 resync frame 數（room 相符的）
	fetches []resyncFetch
	wg      sync.WaitGroup
}

func newResyncFetcher(ctx context.Context, delayMax time.Duration, fetch resyncFetchFunc) *resyncFetcher {
	return &resyncFetcher{ctx: ctx, delayMax: delayMax, fetchFn: fetch, pending: map[int]bool{}, resyncs: map[int]int{}}
}

// 判斷一個 WS frame 是不是 room 的 resync：{"type":"resync","payload":{"room":"<public id>"}}。
// 先用字串快速排除，省得每個 message frame 都做 JSON parse
var resyncTypeNeedle = []byte(`"type":"resync"`)

func isResyncFrame(room string, msg []byte) bool {
	if !bytes.Contains(msg, resyncTypeNeedle) {
		return false
	}
	var env struct {
		Type    string `json:"type"`
		Payload struct {
			Room string `json:"room"`
		} `json:"payload"`
	}
	return json.Unmarshal(msg, &env) == nil && env.Type == "resync" && env.Payload.Room == room
}

// 處理觀眾 viewer（連在 server、在房間 room）收到的一個 WS frame；只有 room 相符的 resync frame 有作用
// （計入該觀眾的 resync 數，並照上面的規則決定要不要排補撈）。要在 reader goroutine 裡同步呼叫，不會阻塞：
// wg.Add 在這裡做，viewer 全關掉之後 Wait 才算得完整
func (f *resyncFetcher) OnFrame(viewer int, server, room string, msg []byte, at time.Time) {
	if !isResyncFrame(room, msg) {
		return
	}
	f.mu.Lock()
	f.resyncs[viewer]++
	if f.pending[viewer] {
		f.mu.Unlock()
		return
	}
	f.pending[viewer] = true
	f.mu.Unlock()

	f.wg.Add(1)
	go f.run(viewer, server, room, at)
}

// 等延遲、清掉 pending、GET、記錄
func (f *resyncFetcher) run(viewer int, server, room string, scheduled time.Time) {
	defer f.wg.Done()
	if !f.sleep(f.nextDelay()) { // ctx 取消（Ctrl-C）：放棄還在等的補撈
		f.setPending(viewer, false)
		return
	}
	f.setPending(viewer, false) // GET 之前清掉：已經開始的補撈不擋新的 resync
	start := f.now()
	status, err := f.fetchFn(f.ctx, server, room)
	rec := resyncFetch{Viewer: viewer, Server: server, Room: room, Scheduled: scheduled, Start: start, Duration: f.now().Sub(start), Status: status}
	if err != nil {
		rec.Status, rec.Err = status, errKey(err)
	}
	f.mu.Lock()
	f.fetches = append(f.fetches, rec)
	f.mu.Unlock()
}

func (f *resyncFetcher) setPending(viewer int, v bool) {
	f.mu.Lock()
	f.pending[viewer] = v
	f.mu.Unlock()
}

func (f *resyncFetcher) nextDelay() time.Duration {
	if f.delayFn != nil {
		return f.delayFn()
	}
	if f.delayMax <= 0 {
		return 0
	}
	return rand.N(f.delayMax)
}

func (f *resyncFetcher) sleep(d time.Duration) bool {
	if f.sleepFn != nil {
		return f.sleepFn(f.ctx, d)
	}
	if d <= 0 {
		return f.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-f.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (f *resyncFetcher) now() time.Time {
	if f.nowFn != nil {
		return f.nowFn()
	}
	return time.Now()
}

// 等所有排定或進行中的補撈結束。要在關掉全部 viewer（不會再有 OnFrame）之後呼叫
func (f *resyncFetcher) Wait() { f.wg.Wait() }

// 目前的紀錄（依 Start 排序）與每條觀眾的 resync 數；都是複本。通常在 Wait 之後呼叫
func (f *resyncFetcher) Snapshot() (fetches []resyncFetch, resyncs map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fetches = slices.Clone(f.fetches)
	slices.SortStableFunc(fetches, func(a, b resyncFetch) int { return a.Start.Compare(b.Start) })
	resyncs = make(map[int]int, len(f.resyncs))
	for k, v := range f.resyncs {
		resyncs[k] = v
	}
	return fetches, resyncs
}

// 錯誤訊息當分類 key：net/http 的 *url.Error 訊息帶整個 URL（含房間 id，每個房間都不一樣），要拆掉內層錯誤才能歸成一類
func errKey(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// 真的去 GET 歷史的 resyncFetchFunc：重用 fetchHistorySeqs（跟 web client 一樣的 GET /rooms/{id}/messages，不帶 token）。
// 它會順便 parse 回應找 marker（這裡用不到，成本很小）；回應無法解析算 status 200 + 錯誤
func httpResyncFetch(client *http.Client, run string) resyncFetchFunc {
	return func(ctx context.Context, server, room string) (int, error) {
		_, err := fetchHistorySeqs(ctx, client, server, room, run)
		var se *historyStatusError
		switch {
		case err == nil:
			return http.StatusOK, nil
		case errors.As(err, &se):
			return se.Status, nil
		case errors.Is(err, errDecodeHistory):
			return http.StatusOK, err
		}
		return 0, err
	}
}

// ---- 報告（純計算）----

const (
	spikeBucket        = 100 * time.Millisecond
	spikeBucketsPerSec = int(time.Second / spikeBucket)
)

type spikeConfigJSON struct {
	Rooms          int     `json:"rooms"`
	ViewersPerRoom int     `json:"viewers_per_room"`
	ViewersTotal   int     `json:"viewers_total"`
	RoomRate       float64 `json:"room_rate_msg_per_sec"`
	TotalRate      float64 `json:"total_rate_msg_per_sec"`
	ResyncDelayMax string  `json:"resync_delay_max"`
	DurationS      float64 `json:"duration_s"`
}

// 延遲摘要（毫秒）。比 latencySummary 多 p90（這個情境要看的百分位），沒有 p95
type spikeLatency struct {
	N     int     `json:"n"`
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

type spikeGets struct {
	Total    int            `json:"total"`
	OK       int            `json:"ok"`
	Failed   int            `json:"failed"`
	FailedBy map[string]int `json:"failed_by,omitempty"` // key：HTTP status 或 "err: <訊息>"
	Latency  spikeLatency   `json:"latency_ok"`          // 只算成功的 GET
}

type spikeTimeline struct {
	FirstGetStartS *float64 `json:"first_get_start_s,omitempty"` // 相對 run 起點（poster 的排程起點）
	LastGetStartS  *float64 `json:"last_get_start_s,omitempty"`
	Per100ms       []int    `json:"gets_per_100ms"` // 第 i 格 = 第一個 GET 開始後 [i*100ms, (i+1)*100ms) 內開始的 GET 數
	PeakPer100ms   int      `json:"peak_per_100ms"`
	PeakPer1s      int      `json:"peak_per_1s"`
}

type spikeDistribution struct {
	ResyncFrames map[int]int `json:"resync_frames_to_viewers"` // 收到幾個 resync frame → 有幾條觀眾（含 0）
	Gets         map[int]int `json:"gets_to_viewers"`          // 做了幾次 GET → 有幾條觀眾（含 0）
}

type spikeServerSummary struct {
	Server             string          `json:"server"`
	Viewers            int             `json:"viewers"`
	ResyncSentDelta    float64         `json:"metric_resync_sent_delta"`
	PublishFailedDelta float64         `json:"metric_publish_failed_delta"`
	MetricSamplesOK    int             `json:"metric_samples_ok"`
	MetricSampleErrors int             `json:"metric_sample_errors"`
	Posts              *redisPostStats `json:"posts,omitempty"`
	Disconnects        int             `json:"disconnects"` // 這台 server 上非預期斷線的觀眾數
}

type spikeReport struct {
	Start     string               `json:"start"` // t_s = 0：poster 的排程起點
	Config    spikeConfigJSON      `json:"config"`
	Gets      spikeGets            `json:"gets"`
	Timeline  spikeTimeline        `json:"timeline"`
	PerViewer spikeDistribution    `json:"per_viewer"`
	Servers   []spikeServerSummary `json:"servers"`
	// 非預期斷線的觀眾總數（每條觀眾最多算一次；收尾的 closeAll 不算）
	Disconnects int    `json:"disconnects"`
	Note        string `json:"note"`
}

// 記錄非預期斷線的觀眾（idx → 連的 server），每條觀眾只記第一次，語意同 redisObserver.onClose；可被多個 reader goroutine 同時呼叫
type spikeDisconnects struct {
	mu sync.Mutex
	m  map[int]string
}

// 給 viewerConfig.OnClose 用：viewerPool 只在非預期斷線時才呼叫（closeAll 收尾不算）
func (d *spikeDisconnects) onClose(v *viewer, _ error, _ time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[int]string{}
	}
	if _, ok := d.m[v.idx]; !ok {
		d.m[v.idx] = v.server
	}
}

// 目前記到的斷線（複製一份）
func (d *spikeDisconnects) snapshot() map[int]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.m)
}

// 一次 run 的輸入（純資料，測試用手造的值）
type spikeInput struct {
	Start   time.Time
	Config  spikeConfigJSON
	Viewers map[int]string // 成功 join 的觀眾 idx → 連的 server
	Servers []string
	Fetches []resyncFetch
	Resyncs map[int]int
	Posts   map[string]*redisPostStats // 依 server 的 POST 結果（可為 nil）
	Samples []redisMetricSample
	SampErr map[string]int
	// 非預期斷線的觀眾 idx → 連的 server（可為 nil）
	Disconnects map[int]string
}

// 成功 GET 的延遲摘要（nearest-rank 百分位，用 percentile）；沒資料回零值
func summarizeSpikeLatency(durs []time.Duration) spikeLatency {
	if len(durs) == 0 {
		return spikeLatency{}
	}
	sorted := slices.Clone(durs)
	slices.Sort(sorted)
	return spikeLatency{
		N: len(sorted), P50Ms: durMs(percentile(sorted, 50)), P90Ms: durMs(percentile(sorted, 90)),
		P99Ms: durMs(percentile(sorted, 99)), MaxMs: durMs(sorted[len(sorted)-1]),
	}
}

// 把開始時間依 width 分桶，原點是最早的那一筆；回傳每桶的筆數（長度到最晚那筆所在的桶）。沒資料回 nil
func bucketStarts(starts []time.Time, width time.Duration) []int {
	if len(starts) == 0 {
		return nil
	}
	first, last := starts[0], starts[0]
	for _, s := range starts {
		if s.Before(first) {
			first = s
		}
		if s.After(last) {
			last = s
		}
	}
	out := make([]int, int(last.Sub(first)/width)+1)
	for _, s := range starts {
		out[int(s.Sub(first)/width)]++
	}
	return out
}

// 任意連續 n 格的總和的最大值（n 超過長度就是全部相加）；沒資料回 0
func peakWindow(buckets []int, n int) int {
	best, sum := 0, 0
	for i, c := range buckets {
		sum += c
		if i >= n {
			sum -= buckets[i-n]
		}
		best = max(best, sum)
	}
	return best
}

// 「次數 → 有幾條觀眾」。viewers 是全部成功 join 的觀眾 idx，沒出現在 counts 的算 0 次
func countDistribution(viewers []int, counts map[int]int) map[int]int {
	out := map[int]int{}
	for _, v := range viewers {
		out[counts[v]]++
	}
	return out
}

func buildSpikeReport(in spikeInput) spikeReport {
	rep := spikeReport{Start: in.Start.UTC().Format(time.RFC3339Nano), Config: in.Config}

	var okLat []time.Duration
	var starts []time.Time
	getsByViewer := map[int]int{}
	for _, f := range in.Fetches {
		rep.Gets.Total++
		starts = append(starts, f.Start)
		getsByViewer[f.Viewer]++
		if f.ok() {
			rep.Gets.OK++
			okLat = append(okLat, f.Duration)
			continue
		}
		rep.Gets.Failed++
		if rep.Gets.FailedBy == nil {
			rep.Gets.FailedBy = map[string]int{}
		}
		rep.Gets.FailedBy[f.failKey()]++
	}
	rep.Gets.Latency = summarizeSpikeLatency(okLat)

	rep.Timeline.Per100ms = bucketStarts(starts, spikeBucket)
	rep.Timeline.PeakPer100ms = peakWindow(rep.Timeline.Per100ms, 1)
	rep.Timeline.PeakPer1s = peakWindow(rep.Timeline.Per100ms, spikeBucketsPerSec)
	if len(starts) > 0 {
		lo, hi := slices.MinFunc(starts, time.Time.Compare), slices.MaxFunc(starts, time.Time.Compare)
		a, b := relS(lo, in.Start), relS(hi, in.Start)
		rep.Timeline.FirstGetStartS, rep.Timeline.LastGetStartS = &a, &b
	}

	idxs := make([]int, 0, len(in.Viewers))
	for idx := range in.Viewers {
		idxs = append(idxs, idx)
	}
	rep.PerViewer = spikeDistribution{ResyncFrames: countDistribution(idxs, in.Resyncs), Gets: countDistribution(idxs, getsByViewer)}

	for _, s := range in.Servers {
		ss := spikeServerSummary{Server: s, Posts: in.Posts[s], MetricSampleErrors: in.SampErr[s]}
		for _, srv := range in.Viewers {
			if srv == s {
				ss.Viewers++
			}
		}
		if ms := samplesOfServer(in.Samples, s); len(ms) > 0 {
			first, last := ms[0], ms[len(ms)-1]
			ss.MetricSamplesOK = len(ms)
			ss.ResyncSentDelta = last.ResyncSent - first.ResyncSent
			ss.PublishFailedDelta = last.PublishFailed - first.PublishFailed
		}
		for _, srv := range in.Disconnects {
			if srv == s {
				ss.Disconnects++
			}
		}
		rep.Disconnects += ss.Disconnects
		rep.Servers = append(rep.Servers, ss)
	}
	rep.Note = "gets = history GETs made after a resync frame (each viewer waits a random [0, resync_delay_max) first; a resync arriving while one is pending is ignored, one arriving after the GET started schedules another); latency_ok = GET start to body read, ok GETs only (status 200); failed_by key = HTTP status or err: <message>. " +
		"gets_per_100ms is bucketed from the first GET start; first/last_get_start_s are relative to start (the poster's schedule origin); peak_per_100ms = max bucket, peak_per_1s = max sum of any 10 consecutive 100ms buckets. " +
		"per_viewer maps a count to the number of viewers with that count (0 included). servers[].metric_*_delta = last minus first /metrics sample over the whole run; posts = POST outcomes by target server (all rooms). " +
		"GET latency includes reading and JSON-decoding the response body (tool side, microseconds). GETs use the shared client timeout -post-timeout (default 10s); timed-out GETs appear only under failed_by, not in latency. " +
		"disconnects = viewers whose WS closed unexpectedly during the run (the final closeAll is not counted, each viewer at most once, no reconnect); viewers with 0 resyncs / 0 GETs may be explained by a disconnect."
	return rep
}

// 取出某台 server 的 /metrics 樣本（保持原本時間順序）；沒有就回 nil。redis 與 spike 報告共用，算首尾差值與筆數
func samplesOfServer(samples []redisMetricSample, server string) []redisMetricSample {
	var out []redisMetricSample
	for _, m := range samples {
		if m.Server == server {
			out = append(out, m)
		}
	}
	return out
}

// 給人看的摘要（完整資料在 JSON）
func printSpike(w io.Writer, r *spikeReport) {
	c := r.Config
	fmt.Fprintf(w, "\nscenario=resync-spike  start=%s  rooms=%d viewers/room=%d viewers=%d  room_rate=%.1f msg/s (total %.0f)  resync_delay_max=%s  duration=%.0fs\n",
		r.Start, c.Rooms, c.ViewersPerRoom, c.ViewersTotal, c.RoomRate, c.TotalRate, c.ResyncDelayMax, c.DurationS)
	g := r.Gets
	fmt.Fprintf(w, "gets: total=%d ok=%d failed=%d (%s)  latency_ok p50=%.1fms p90=%.1fms p99=%.1fms max=%.1fms\n",
		g.Total, g.OK, g.Failed, formatReasons(g.FailedBy), g.Latency.P50Ms, g.Latency.P90Ms, g.Latency.P99Ms, g.Latency.MaxMs)
	t := r.Timeline
	if t.FirstGetStartS == nil {
		fmt.Fprintln(w, "timeline: no GET was made")
	} else {
		fmt.Fprintf(w, "timeline: first_get=t+%.2fs last_get=t+%.2fs peak=%d/100ms %d/1s\n", *t.FirstGetStartS, *t.LastGetStartS, t.PeakPer100ms, t.PeakPer1s)
	}
	fmt.Fprintf(w, "per viewer: resync frames -> viewers: %s ; gets -> viewers: %s\n", formatDist(r.PerViewer.ResyncFrames), formatDist(r.PerViewer.Gets))
	for _, sv := range r.Servers {
		fmt.Fprintf(w, "%s: viewers=%d metrics: resync_sent+%.0f publish_failed+%.0f", sv.Server, sv.Viewers, sv.ResyncSentDelta, sv.PublishFailedDelta)
		if p := sv.Posts; p != nil {
			fmt.Fprintf(w, "  posts sent=%d status: %s", p.Sent, formatReasons(p.StatusCounts))
		}
		if sv.Disconnects > 0 {
			fmt.Fprintf(w, "  disconnects=%d", sv.Disconnects)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "disconnects (unexpected WS closes): %d\n", r.Disconnects)
}

// {0:3, 1:97} → "0:3 1:97"（依次數遞增）
func formatDist(m map[int]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d:%d", k, m[k])
	}
	return s
}

// ---- 執行 ----

// 跑 resync-spike 情境。回傳 exit code（0 / 1 / 130）；資料清理由呼叫端 main.run 的 defer 負責
func runSpike(ctx context.Context, o *options, client *http.Client, servers []string, data *seeded, runTag string, rep *runReport) int {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	fetcher := newResyncFetcher(runCtx, o.redisResyncDelayMax, httpResyncFetch(client, runTag))
	disc := &spikeDisconnects{}
	vp := newViewerPool(viewerConfig{
		Servers: servers, Rooms: data.rooms, Run: runTag, RampRate: o.rampRate, JoinTimeout: 10 * time.Second, MaxDialing: 128,
		OnFrame: func(v *viewer, msg []byte, at time.Time) {
			fetcher.OnFrame(v.idx, v.server, data.rooms[v.roomIdx], msg, at)
		},
		OnClose: disc.onClose,
	})
	defer vp.closeAll()

	total := o.spikeRooms * o.spikeViewersPerRoom
	ramp := vp.rampTo(ctx, total)
	fmt.Printf("resync-spike: joined %d/%d viewers (%d rooms x %d), rejected %d (%s)\n", ramp.Joined, total, o.spikeRooms, o.spikeViewersPerRoom, ramp.Rejected, formatReasons(ramp.Reasons))
	if ramp.Joined != total {
		return fail1("not every viewer joined; refusing to run a partial experiment")
	}

	start := time.Now().Add(500 * time.Millisecond)
	stopSamplers := startMetricSamplers(ctx, client, servers, start)

	rate := float64(o.spikeRooms) * o.spikeRoomRate
	fmt.Printf("resync-spike: %d rooms x %d viewers, %.0f msg/s total for %s (start Redis fault now via redis.sh)\n", o.spikeRooms, o.spikeViewersPerRoom, rate, o.duration)
	p := &poster{client: client, servers: servers, rooms: data.rooms, tokens: data.tokens, run: runTag}
	loop := runOpenLoop(runCtx, openLoopConfig{Start: start, Rate: rate, Measure: o.duration, MaxInFlight: o.maxInFlight, Send: p.send})
	sleepCtx(ctx, o.drain)

	samples, sampleErrs := stopSamplers()
	vp.closeAll()  // 先關 reader（之後不會再 OnFrame），再等補撈
	fetcher.Wait() // 進行中的補撈讓它跑完（client 有 timeout）
	fetches, resyncs := fetcher.Snapshot()

	viewers := map[int]string{}
	for _, v := range vp.joinedViewers() {
		viewers[v.idx] = v.server
	}
	report := buildSpikeReport(spikeInput{
		Start: start,
		Config: spikeConfigJSON{
			Rooms: o.spikeRooms, ViewersPerRoom: o.spikeViewersPerRoom, ViewersTotal: total, RoomRate: o.spikeRoomRate,
			TotalRate: rate, ResyncDelayMax: o.redisResyncDelayMax.String(), DurationS: o.duration.Seconds(),
		},
		Viewers: viewers, Servers: servers, Fetches: fetches, Resyncs: resyncs,
		Posts:   buildPostStatsByServerRooms(loop, servers, len(data.rooms)),
		Samples: samples, SampErr: sampleErrs, Disconnects: disc.snapshot(),
	})
	rep.Spike = &report
	if ctx.Err() != nil {
		rep.Aborted = true
		rep.Notes = append(rep.Notes, "interrupted: the run was cut short")
		return 130
	}
	return 0
}
