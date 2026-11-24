package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/usecase"
)

// -scenario redis：Redis 在跑到一半時被外部（deploy/fault/redis.sh）stop / pause，觀察 POST、WS 漏收、resync 與補撈。
// 每台 server 各 -redis-viewers 條 WS 觀眾進同一個房間，以 -redis-rate 持續發帶序號 marker 的彈幕（step 固定 0）。
// 觀眾不重連；收到 resync frame 時照 web client 的做法：等 -redis-resync-delay-min 加隨機 [0, -redis-resync-delay-max) 再 GET /rooms/{id}/messages（最新歷史），把撈到的 marker 另外記成「補回」。
// 已有等待中的補撈就忽略後來的 resync（它會在之後才觸發，已涵蓋）；補撈已經開始的不算，仍會另排一次。
// 整段期間每秒讀每台 server 的 /metrics。這個情境不做有效性判定（沒有 CPU 檢查），只收資料。

const (
	metricResyncSent    = "websocket_resync_sent_total"
	metricPublishFailed = "redis_publish_failed_total"
	metricPubSubDeaf    = "redis_pubsub_deaf_total"
	metricWSViewers     = "websocket_viewers" // 帶 auth label，見 parseViewersByAuth
)

var redisMetricNames = []string{metricWSConnections, metricResyncSent, metricPublishFailed, metricPubSubDeaf}

type redisFetch struct {
	At    time.Time
	Count int    // 撈到的 marker 數
	Err   string // 空 = 成功
	// 撈到的 marker 中最小 / 最大的 seq；Count == 0 時沒有意義（0），輸出成 JSON 時會省略（見 redisFetchJSON）
	MinSeq, MaxSeq int
}

type redisDisconnect struct {
	At     time.Time
	Reason string
}

// 一條觀眾的收件紀錄。reader goroutine 與 resync 補撈 goroutine 都會寫，所以要鎖
type redisViewerState struct {
	idx    int
	server string

	mu         sync.Mutex
	ws         map[int]time.Time // seq → 第一次從 WS 收到的時間
	recovered  map[int]struct{}  // 補撈（GET）拿到的 seq
	resyncs    []time.Time
	fetches    []redisFetch
	presence   []presenceFrame // room 相符的 presence frame（kill.go 的 presenceFrame）
	disconnect *redisDisconnect

	// 已排定、還沒開始的延遲補撈；為 true 時後來的 resync 不再排
	fetchPending bool
}

func newRedisViewerState(idx int, server string) *redisViewerState {
	return &redisViewerState{idx: idx, server: server, ws: map[int]time.Time{}, recovered: map[int]struct{}{}}
}

// 處理一個 WS frame：message frame 的 marker 記進 ws；room 相符的 resync frame 記下時間並回傳 true（呼叫端負責補撈）。
// 其他 frame（presence、別人的 marker、別的房間的 resync）一律忽略
func (s *redisViewerState) handleFrame(run, room string, msg []byte, at time.Time) (resync bool) {
	var env struct {
		Type    string `json:"type"`
		Payload struct {
			Room    string `json:"room"`
			Content string `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(msg, &env) != nil {
		return false
	}
	switch env.Type {
	case "message":
		step, seq, ok := parseMarker(run, env.Payload.Content)
		if !ok || step != 0 {
			return false
		}
		s.mu.Lock()
		if _, dup := s.ws[seq]; !dup {
			s.ws[seq] = at
		}
		s.mu.Unlock()
	case "resync":
		if env.Payload.Room != room {
			return false
		}
		s.mu.Lock()
		s.resyncs = append(s.resyncs, at)
		s.mu.Unlock()
		return true
	}
	return false
}

func (s *redisViewerState) addPresence(at time.Time, count int) {
	s.mu.Lock()
	s.presence = append(s.presence, presenceFrame{At: at, Count: count})
	s.mu.Unlock()
}

// 嘗試把這條觀眾標成「有補撈等待中」；已經在等就回傳 false（呼叫端不用再排）
func (s *redisViewerState) tryMarkPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fetchPending {
		return false
	}
	s.fetchPending = true
	return true
}

func (s *redisViewerState) clearPending() {
	s.mu.Lock()
	s.fetchPending = false
	s.mu.Unlock()
}

func (s *redisViewerState) addFetch(at time.Time, seqs []int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := redisFetch{At: at, Count: len(seqs)}
	if len(seqs) > 0 {
		f.MinSeq, f.MaxSeq = slices.Min(seqs), slices.Max(seqs)
	}
	if err != nil {
		f.Err = err.Error()
	}
	s.fetches = append(s.fetches, f)
	for _, q := range seqs {
		s.recovered[q] = struct{}{}
	}
}

// 歷史 GET 的回應無法解析；不是網路問題，重試沒用（kill 情境的重試政策用它判斷）
var errDecodeHistory = errors.New("decode history")

// 歷史 GET 拿到非 200 的 status；kill 情境的重試政策依 Status 判斷（5xx 重試、4xx 不重試）
type historyStatusError struct{ Status int }

func (e *historyStatusError) Error() string { return fmt.Sprintf("GET history returned %d", e.Status) }

// 解析 GET /rooms/{id}/messages 的回應（訊息陣列），挑出這次 run 的 marker 序號
func parseHistorySeqs(run string, body []byte) ([]int, error) {
	var msgs []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, fmt.Errorf("%w: %v", errDecodeHistory, err)
	}
	var seqs []int
	for _, m := range msgs {
		if step, seq, ok := parseMarker(run, m.Content); ok && step == 0 {
			seqs = append(seqs, seq)
		}
	}
	return seqs, nil
}

// 和 web client 的 fetchMessages 一樣：GET /rooms/{public_id}/messages（沒有參數，server 回最新一批；這支 GET 不需要 token）
func fetchHistorySeqs(ctx context.Context, client *http.Client, base, room, run string) ([]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rooms/"+room+"/messages", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &historyStatusError{Status: resp.StatusCode}
	}
	return parseHistorySeqs(run, body)
}

// 最近一次成功且撈到至少一個 marker 的補撈的最小 seq（server 只回最新 HistoryLimit 則，比它舊的補不回來）；沒有則 ok=false
func latestFetchMinSeq(fetches []redisFetch) (minSeq int, ok bool) {
	for i := len(fetches) - 1; i >= 0; i-- {
		if f := fetches[i]; f.Err == "" && f.Count > 0 {
			return f.MinSeq, true
		}
	}
	return 0, false
}

// FinalMissing 中比 fetchMin 還舊的個數（在 HistoryLimit 視窗之外，不可能補回）
func countOlderThan(finalMissing []int, fetchMin int) int {
	n := 0
	for _, q := range finalMissing {
		if q < fetchMin {
			n++
		}
	}
	return n
}

// WS 收到、但不在 201 集合的 seq 個數
func countNot201(ws map[int]time.Time, created []int) int {
	set := make(map[int]struct{}, len(created))
	for _, q := range created {
		set[q] = struct{}{}
	}
	n := 0
	for q := range ws {
		if _, ok := set[q]; !ok {
			n++
		}
	}
	return n
}

// 一條觀眾的結果：created = 有拿到 201 的 seq
type viewerOutcome struct {
	Missing      []int // created 但 WS 沒收到
	Recovered    []int // Missing 中補撈拿到的
	FinalMissing []int // Missing 扣掉 Recovered
}

func computeOutcome(created []int, ws map[int]time.Time, recovered map[int]struct{}) viewerOutcome {
	var o viewerOutcome
	for _, q := range created {
		if _, ok := ws[q]; ok {
			continue
		}
		o.Missing = append(o.Missing, q)
		if _, ok := recovered[q]; ok {
			o.Recovered = append(o.Recovered, q)
		} else {
			o.FinalMissing = append(o.FinalMissing, q)
		}
	}
	return o
}

// [1,2,3,7,9,10] → "1-3,7,9-10"（輸入要遞增）
func seqRanges(seqs []int) string {
	var parts []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(seqs[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", seqs[i], seqs[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// ---- 報告 ----

type redisViewerResult struct {
	Viewer            int                  `json:"viewer"`
	Server            string               `json:"server"`
	ReceivedWS        int                  `json:"received_ws"`
	Missing           int                  `json:"missing"`
	Recovered         int                  `json:"recovered"`
	FinalMissing      int                  `json:"final_missing"`
	FinalMissingOlder int                  `json:"final_missing_older_than_fetch"`
	ReceivedNot201    int                  `json:"received_not_201"`
	MissingRanges     string               `json:"missing_seq_ranges,omitempty"`
	FinalMissingRange string               `json:"final_missing_seq_ranges,omitempty"`
	ResyncAtS         []float64            `json:"resync_at_s,omitempty"`
	ResyncAt          []string             `json:"resync_at,omitempty"`
	Fetches           []redisFetchJSON     `json:"resync_fetches,omitempty"`
	Disconnect        *redisDisconnectJSON `json:"disconnect,omitempty"`
}

type redisFetchJSON struct {
	AtS    float64 `json:"at_s"`
	Count  int     `json:"markers"`
	MinSeq *int    `json:"min_seq,omitempty"`
	MaxSeq *int    `json:"max_seq,omitempty"`
	Err    string  `json:"error,omitempty"`
}

type redisDisconnectJSON struct {
	AtS    float64 `json:"at_s"`
	At     string  `json:"at"`
	Reason string  `json:"reason"`
}

// 一台 server 的彙整
type redisServerSummary struct {
	Server         string   `json:"server"`
	Viewers        int      `json:"viewers"`
	ReceivedWS     int      `json:"received_ws"`
	Missing        int      `json:"missing"`
	Recovered      int      `json:"recovered"`
	FinalMissing   int      `json:"final_missing"`
	ResyncViewers  int      `json:"viewers_resynced"`
	FirstResyncAt  string   `json:"first_resync_at,omitempty"`
	FirstResyncAtS *float64 `json:"first_resync_at_s,omitempty"`
	Disconnects    int      `json:"disconnects"`

	// /metrics：第一筆與最後一筆的差，以及觀察期間 connections 的範圍。resync / publish_failed / deaf 是 counter，差 = 這次 run 內增加的量
	ResyncSentDelta    float64 `json:"metric_resync_sent_delta"`
	PublishFailedDelta float64 `json:"metric_publish_failed_delta"`
	PubSubDeafDelta    float64 `json:"metric_pubsub_deaf_delta"`
	ConnectionsMin     int     `json:"metric_ws_connections_min"`
	ConnectionsMax     int     `json:"metric_ws_connections_max"`
	MetricSamplesOK    int     `json:"metric_samples_ok"`
	MetricSampleErrors int     `json:"metric_sample_errors"`

	// 以下兩個是後來加的（附加欄位）：這台 server 的觀眾看到的 presence，以及 POST 打到這台 server 的結果
	Presence *redisPresenceSummary `json:"presence,omitempty"` // 沒有任何 presence frame 時省略
	Posts    *redisPostStats       `json:"posts,omitempty"`
}

// presence 時間序列的分桶寬度（秒）
const presenceBucketS = 5

// 一個 5 秒桶裡，這台 server 的任一觀眾最後收到的 presence count。TS 是桶起點（相對 start 的秒數，join 在 start 之前的 frame 會落在負數桶）
type presenceBucket struct {
	TS    int `json:"t_s"`
	Count int `json:"count"`
}

// 一台 server 的觀眾看到的 presence：第一 / 最後一筆（依時間）、最小 / 最大值，以及每 5 秒一格的序列（沒有 frame 的桶省略）
type redisPresenceSummary struct {
	Frames int              `json:"frames"`
	First  int              `json:"first"`
	Last   int              `json:"last"`
	Min    int              `json:"min"`
	Max    int              `json:"max"`
	Series []presenceBucket `json:"series_5s"`
}

// POST 依目標 server 的彙整：status_counts 的 key 同 post_timeline（"201" / "500" / "err"（傳輸錯誤或 client timeout，兩者在 sender 不分）/ "dropped"）；
// latency_201 只算 201 的（排程時間 → 回應完成）
type redisPostStats struct {
	Sent         int            `json:"sent"`
	StatusCounts map[string]int `json:"status_counts"`
	Latency201   latencySummary `json:"latency_201"`
	LatencyAll   latencySummary `json:"latency_all"` // 除 dropped 外所有結果（201 / 500 / 其他狀態 / err），看失敗的 POST 卡了多久
}

type postSecond struct {
	TS    int            `json:"t_s"`
	Sent  int            `json:"sent"`
	By    map[string]int `json:"status_counts"` // "201" / "500" / ... / "err"（傳輸錯誤或 timeout）/ "dropped"
	P50Ms float64        `json:"p50_ms"`
	MaxMs float64        `json:"max_ms"`
}

type redisMetricSample struct {
	TS            float64 `json:"t_s"`
	Server        string  `json:"server"`
	Connections   int     `json:"ws_connections"`
	ResyncSent    float64 `json:"resync_sent_total"`
	PublishFailed float64 `json:"publish_failed_total"`
	PubSubDeaf    float64 `json:"pubsub_deaf_total"`
	// websocket_viewers{auth="member"|"anonymous"}：這台 server 的登入（不重複 user）與匿名連線數
	ViewersMember    int `json:"ws_viewers_member"`
	ViewersAnonymous int `json:"ws_viewers_anonymous"`

	at time.Time
}

type redisSummary struct {
	Start             string         `json:"start"` // t_s = 0：poster 的排程起點
	Rate              float64        `json:"rate_msg_per_sec"`
	ResyncDelayMin    string         `json:"resync_delay_min"` // 觀眾收到 resync 後固定等待的時間（min 與 max 都是 "0s" = 立刻撈）
	ResyncDelayMax    string         `json:"resync_delay_max"` // 固定等待之後再加的隨機延遲上限（[0, max)）
	DurationS         float64        `json:"duration_s"`
	Viewers           int            `json:"viewers"`
	Posts             int            `json:"posts_scheduled"`
	PostsCreated      int            `json:"posts_201"`
	PostStatus        map[string]int `json:"post_status_total"`
	ReceivedWS        int            `json:"received_ws"`
	Missing           int            `json:"missing"`
	Recovered         int            `json:"recovered"`
	FinalMissing      int            `json:"final_missing"`
	FinalMissingOlder int            `json:"final_missing_older_than_fetch"`
	ReceivedNot201    int            `json:"received_not_201"`
	HistoryLimit      int            `json:"history_limit"`
	Disconnects       int            `json:"disconnects"`
	Note              string         `json:"note"`
}

type redisReport struct {
	Summary  redisSummary         `json:"summary"`
	Servers  []redisServerSummary `json:"servers"`
	Viewers  []redisViewerResult  `json:"viewers"`
	Timeline []postSecond         `json:"post_timeline"`
	Metrics  []redisMetricSample  `json:"metric_samples"`
}

func statusKey(outcome int8, status int32) string {
	switch outcome {
	case outcomeResponse:
		return strconv.Itoa(int(status))
	case outcomeError:
		return "err"
	case outcomeDropped:
		return "dropped"
	}
	return ""
}

// 每秒（依排程時間分桶）的 POST 狀態計數與延遲（p50 / max，延遲從排程時間算到回應或錯誤）
func buildPostTimeline(res *openLoopResult) []postSecond {
	var out []postSecond
	lat := map[int][]time.Duration{}
	for seq := range res.Total {
		k := statusKey(res.Outcome[seq], res.Status[seq])
		if k == "" {
			continue
		}
		sec := int(res.schedOffset(seq) / time.Second)
		for len(out) <= sec {
			out = append(out, postSecond{TS: len(out), By: map[string]int{}})
		}
		out[sec].Sent++
		out[sec].By[k]++
		if res.Outcome[seq] != outcomeDropped {
			lat[sec] = append(lat[sec], time.Duration(res.LatencyNs[seq]))
		}
	}
	for sec := range out {
		if l := lat[sec]; len(l) > 0 {
			s := summarize(l)
			out[sec].P50Ms, out[sec].MaxMs = s.P50Ms, s.MaxMs
		}
	}
	return out
}

// 彙整一台 server 的 presence frame（所有觀眾的，任意順序）；沒有 frame 回 nil。純計算
// first / min / max 只看 At >= start 的 frame（start 之前的是 join 時的暫態）；start 之後沒有 frame 時三者為 0。last 是整體最後一筆。series_5s 含 start 之前的負數桶
func summarizePresenceSeries(frames []presenceFrame, start time.Time) *redisPresenceSummary {
	if len(frames) == 0 {
		return nil
	}
	sorted := slices.Clone(frames)
	slices.SortStableFunc(sorted, func(a, b presenceFrame) int { return a.At.Compare(b.At) })
	sum := &redisPresenceSummary{Frames: len(sorted), Last: sorted[len(sorted)-1].Count}
	seen := false
	for _, f := range sorted {
		if !f.At.Before(start) {
			if !seen {
				sum.First, sum.Min, sum.Max = f.Count, f.Count, f.Count
				seen = true
			} else {
				sum.Min, sum.Max = min(sum.Min, f.Count), max(sum.Max, f.Count)
			}
		}
		// floor 除法：負的位移要落在負數桶，不能被截向 0
		b := int(math.Floor(f.At.Sub(start).Seconds()/presenceBucketS)) * presenceBucketS
		if n := len(sum.Series); n > 0 && sum.Series[n-1].TS == b {
			sum.Series[n-1].Count = f.Count // 時間排序，後來的覆蓋 = 該桶最後一筆
		} else {
			sum.Series = append(sum.Series, presenceBucket{TS: b, Count: f.Count})
		}
	}
	return sum
}

// 依目標 server 彙整 POST 結果。每筆 seq 的 server 用 poster 同一個公式（redis 情境只有 1 個房間）
func buildPostStatsByServer(res *openLoopResult, servers []string) map[string]*redisPostStats {
	return buildPostStatsByServerRooms(res, servers, 1)
}

// 同上，但 poster 輪流用 rooms 個房間時 seq → server 是 serverIndex(seq, rooms, len(servers))，所以要知道房間數
func buildPostStatsByServerRooms(res *openLoopResult, servers []string, rooms int) map[string]*redisPostStats {
	out := map[string]*redisPostStats{}
	lat := map[string][]time.Duration{}
	latAll := map[string][]time.Duration{}
	for _, s := range servers {
		out[s] = &redisPostStats{StatusCounts: map[string]int{}}
	}
	for seq := range res.Total {
		k := statusKey(res.Outcome[seq], res.Status[seq])
		if k == "" {
			continue
		}
		s := servers[serverIndex(seq, rooms, len(servers))]
		out[s].Sent++
		out[s].StatusCounts[k]++
		if res.Outcome[seq] != outcomeDropped {
			latAll[s] = append(latAll[s], time.Duration(res.LatencyNs[seq]))
		}
		if res.Outcome[seq] == outcomeResponse && res.Status[seq] == http.StatusCreated {
			lat[s] = append(lat[s], time.Duration(res.LatencyNs[seq]))
		}
	}
	for s, l := range lat {
		out[s].Latency201 = summarize(l)
	}
	for s, l := range latAll {
		out[s].LatencyAll = summarize(l)
	}
	return out
}

func createdSeqs(res *openLoopResult) []int {
	var out []int
	for seq := range res.Total {
		if res.Outcome[seq] == outcomeResponse && res.Status[seq] == http.StatusCreated {
			out = append(out, seq)
		}
	}
	return out
}

func relS(t, start time.Time) float64 { return t.Sub(start).Seconds() }

// 組報告：純計算
func buildRedisReport(start time.Time, rate float64, res *openLoopResult, states []*redisViewerState, servers []string, samples []redisMetricSample, sampleErrs map[string]int) redisReport {
	created := createdSeqs(res)
	rep := redisReport{Timeline: buildPostTimeline(res)}
	sum := redisSummary{
		Start: start.UTC().Format(time.RFC3339Nano), Rate: rate, Viewers: len(states), Posts: res.Total,
		PostsCreated: len(created), PostStatus: map[string]int{},
		DurationS:    float64(res.Total) * res.Interval.Seconds(),
		HistoryLimit: usecase.HistoryLimit,
		Note:         fmt.Sprintf("missing = seqs that got 201 minus WS-received; recovered = missing found in a resync GET; the GET returns only the newest history_limit (%d) messages of the room, so final_missing_older_than_fetch = final_missing seqs older than the min seq of the latest completed fetch (outside that fetch's window; may also include messages committed after an earlier fetch); received_not_201 = WS-received seqs not in the 201 set (e.g. a POST that timed out client-side but committed)", usecase.HistoryLimit),
	}
	for seq := range res.Total {
		if k := statusKey(res.Outcome[seq], res.Status[seq]); k != "" {
			sum.PostStatus[k]++
		}
	}
	byServer := map[string]*redisServerSummary{}
	presenceFrames := map[string][]presenceFrame{}
	for _, s := range servers {
		byServer[s] = &redisServerSummary{Server: s}
	}
	slices.SortFunc(states, func(a, b *redisViewerState) int { return a.idx - b.idx })
	for _, st := range states {
		st.mu.Lock()
		out := computeOutcome(created, st.ws, st.recovered)
		vr := redisViewerResult{
			Viewer: st.idx, Server: st.server, ReceivedWS: len(st.ws),
			Missing: len(out.Missing), Recovered: len(out.Recovered), FinalMissing: len(out.FinalMissing),
			MissingRanges: seqRanges(out.Missing), FinalMissingRange: seqRanges(out.FinalMissing),
			ReceivedNot201: countNot201(st.ws, created),
		}
		if fm, ok := latestFetchMinSeq(st.fetches); ok {
			vr.FinalMissingOlder = countOlderThan(out.FinalMissing, fm)
		}
		for _, t := range st.resyncs {
			vr.ResyncAtS = append(vr.ResyncAtS, relS(t, start))
			vr.ResyncAt = append(vr.ResyncAt, t.UTC().Format(time.RFC3339Nano))
		}
		for _, f := range st.fetches {
			fj := redisFetchJSON{AtS: relS(f.At, start), Count: f.Count, Err: f.Err}
			if f.Count > 0 {
				minSeq, maxSeq := f.MinSeq, f.MaxSeq
				fj.MinSeq, fj.MaxSeq = &minSeq, &maxSeq
			}
			vr.Fetches = append(vr.Fetches, fj)
		}
		if d := st.disconnect; d != nil {
			vr.Disconnect = &redisDisconnectJSON{AtS: relS(d.At, start), At: d.At.UTC().Format(time.RFC3339Nano), Reason: d.Reason}
		}
		firstResync := time.Time{}
		if len(st.resyncs) > 0 {
			firstResync = st.resyncs[0]
		}
		presenceFrames[st.server] = append(presenceFrames[st.server], st.presence...)
		st.mu.Unlock()

		rep.Viewers = append(rep.Viewers, vr)
		sum.ReceivedWS += vr.ReceivedWS
		sum.Missing += vr.Missing
		sum.Recovered += vr.Recovered
		sum.FinalMissing += vr.FinalMissing
		sum.FinalMissingOlder += vr.FinalMissingOlder
		sum.ReceivedNot201 += vr.ReceivedNot201
		if s := byServer[st.server]; s != nil {
			s.Viewers++
			s.ReceivedWS += vr.ReceivedWS
			s.Missing += vr.Missing
			s.Recovered += vr.Recovered
			s.FinalMissing += vr.FinalMissing
			if vr.Disconnect != nil {
				s.Disconnects++
				sum.Disconnects++
			}
			if !firstResync.IsZero() {
				s.ResyncViewers++
				if s.FirstResyncAtS == nil || relS(firstResync, start) < *s.FirstResyncAtS {
					v := relS(firstResync, start)
					s.FirstResyncAtS = &v
					s.FirstResyncAt = firstResync.UTC().Format(time.RFC3339Nano)
				}
			}
		}
	}
	// 每台 server 的 /metrics 彙整（samples 已依時間排序）
	for _, s := range servers {
		ss := byServer[s]
		if ms := samplesOfServer(samples, s); len(ms) > 0 {
			first, last := ms[0], ms[len(ms)-1]
			ss.ConnectionsMin, ss.ConnectionsMax = first.Connections, first.Connections
			for _, m := range ms {
				ss.ConnectionsMin = min(ss.ConnectionsMin, m.Connections)
				ss.ConnectionsMax = max(ss.ConnectionsMax, m.Connections)
			}
			ss.MetricSamplesOK = len(ms)
			ss.ResyncSentDelta = last.ResyncSent - first.ResyncSent
			ss.PublishFailedDelta = last.PublishFailed - first.PublishFailed
			ss.PubSubDeafDelta = last.PubSubDeaf - first.PubSubDeaf
		}
		ss.MetricSampleErrors = sampleErrs[s]
		ss.Presence = summarizePresenceSeries(presenceFrames[s], start)
		rep.Servers = append(rep.Servers, *ss)
	}
	rep.Metrics = samples
	rep.Summary = sum
	return rep
}

// ---- 執行 ----

type redisObserver struct {
	ctx    context.Context
	client *http.Client
	run    string
	room   string

	// 收到 resync 後撈歷史前等 resyncDelayMin + random[0, resyncDelayMax)；兩個都是 0 = 立刻撈（不去重，和舊行為一致）
	resyncDelayMin time.Duration
	resyncDelayMax time.Duration
	// 測試用的注入點：nil 時分別用 rand 與 fetchHistorySeqs
	delayFn func() time.Duration
	fetchFn func(ctx context.Context, server string) ([]int, error)

	mu      sync.Mutex
	states  map[int]*redisViewerState
	fetchWG sync.WaitGroup
}

func (o *redisObserver) state(v *viewer) *redisViewerState {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.states[v.idx]
	if st == nil {
		st = newRedisViewerState(v.idx, v.server)
		o.states[v.idx] = st
	}
	return st
}

func (o *redisObserver) onFrame(v *viewer, msg []byte, at time.Time) {
	st := o.state(v)
	if !st.handleFrame(o.run, o.room, msg, at) {
		return
	}
	// 補撈放在自己的 goroutine，reader 不能被 GET 卡住。
	// fetchWG.Add 一定要在 reader goroutine 裡同步做（closeAll 之後不會再 Add，fetchWG.Wait 才算得完整）
	// 是否走去重路徑只看 resyncDelayMin / resyncDelayMax，不看抽到的延遲（抽到 0 也要清 pending）
	dedupe := o.resyncDelayMin > 0 || o.resyncDelayMax > 0
	delay := time.Duration(0)
	if dedupe {
		if !st.tryMarkPending() {
			return
		}
		delay = o.nextDelay()
	}
	o.fetchWG.Add(1)
	go func() {
		defer o.fetchWG.Done()
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-o.ctx.Done(): // Ctrl-C：放棄還在等的補撈
				t.Stop()
				st.clearPending()
				return
			case <-t.C:
			}
		}
		if dedupe {
			st.clearPending() // GET 之前清掉：已經開始的補撈不擋新的 resync，之後再來的要另排一次
		}
		seqs, err := o.fetch(v.server)
		st.addFetch(time.Now(), seqs, err) // at 是 GET 完成的時間
	}()
}

// resyncDelayMin + [0, resyncDelayMax) 均勻分布的延遲（固定段合併 resync，隨機段攤開 GET）
func (o *redisObserver) nextDelay() time.Duration {
	if o.delayFn != nil {
		return o.delayFn()
	}
	return resyncDelay(o.resyncDelayMin, o.resyncDelayMax)
}

// min + random[0, max)；max <= 0 時沒有隨機段，只剩 min。redisObserver 與 resyncFetcher 共用
func resyncDelay(min, max time.Duration) time.Duration {
	if max <= 0 {
		return min
	}
	return min + rand.N(max)
}

func (o *redisObserver) fetch(server string) ([]int, error) {
	if o.fetchFn != nil {
		return o.fetchFn(o.ctx, server)
	}
	return fetchHistorySeqs(o.ctx, o.client, server, o.room, o.run)
}

func (o *redisObserver) onClose(v *viewer, err error, at time.Time) {
	st := o.state(v)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.disconnect == nil {
		st.disconnect = &redisDisconnect{At: at, Reason: err.Error()}
	}
}

// 從 /metrics 文字挑出 websocket_viewers{auth="..."} 的值，key 是 auth label（member / anonymous）。
// parsePlainMetrics 不處理帶 label 的行，所以另外解析；某個 label 還沒出現（gauge 還沒被建立）就沒有那個 key，讀出來是 0
func parseViewersByAuth(body []byte) map[string]int {
	const prefix = metricWSViewers + `{auth="`
	out := map[string]int{}
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		auth, ok := strings.CutSuffix(strings.TrimPrefix(fields[0], prefix), `"}`)
		v, err := strconv.ParseFloat(fields[1], 64)
		if !ok || err != nil {
			continue
		}
		out[auth] = int(v)
	}
	return out
}

// 每秒讀一台 server 的 /metrics，直到 ctx 取消
func sampleRedisMetrics(ctx context.Context, client *http.Client, base string, start time.Time, out *[]redisMetricSample, errs *int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		at := time.Now()
		var vals map[string]float64
		var viewers map[string]int
		err := fetchMetrics(ctx, client, base, func(r io.Reader) error {
			body, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			if vals, err = parsePlainMetrics(bytes.NewReader(body), redisMetricNames...); err != nil {
				return err
			}
			viewers = parseViewersByAuth(body)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			*errs++
		} else {
			*out = append(*out, redisMetricSample{
				TS: relS(at, start), Server: base, Connections: int(vals[metricWSConnections]),
				ResyncSent: vals[metricResyncSent], PublishFailed: vals[metricPublishFailed], PubSubDeaf: vals[metricPubSubDeaf],
				ViewersMember: viewers["member"], ViewersAnonymous: viewers["anonymous"], at: at,
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// 每台 server 各開一個 goroutine 每秒讀 /metrics；回傳的 stop 會停掉取樣、等它們結束，並回傳依時間排序的樣本與每台的失敗次數。
// redis 與 kill 情境共用（kill 情境的 server 是直連，不經 nginx，被 kill 的那台之後的取樣會失敗，記在 errs）
func startMetricSamplers(ctx context.Context, client *http.Client, servers []string, start time.Time) (stop func() ([]redisMetricSample, map[string]int)) {
	var (
		smu        sync.Mutex
		samples    []redisMetricSample
		sampleErrs = map[string]int{}
		sampleWG   sync.WaitGroup
	)
	sampleCtx, cancelSample := context.WithCancel(ctx)
	for _, s := range servers {
		sampleWG.Add(1)
		go func() {
			defer sampleWG.Done()
			var n int
			var local []redisMetricSample
			sampleRedisMetrics(sampleCtx, client, s, start, &local, &n)
			smu.Lock()
			samples = append(samples, local...)
			sampleErrs[s] += n
			smu.Unlock()
		}()
	}
	return func() ([]redisMetricSample, map[string]int) {
		cancelSample()
		sampleWG.Wait()
		slices.SortFunc(samples, func(a, b redisMetricSample) int { return a.at.Compare(b.at) })
		return samples, sampleErrs
	}
}

// 跑 redis 情境。回傳 exit code（0 / 1 / 130）；資料清理由呼叫端 main.run 的 defer 負責
func runRedis(ctx context.Context, o *options, client *http.Client, servers []string, data *seeded, runTag string, rep *runReport) int {
	room := data.rooms[0]
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	obs := &redisObserver{ctx: runCtx, client: client, run: runTag, room: room, resyncDelayMin: o.redisResyncDelayMin, resyncDelayMax: o.redisResyncDelayMax, states: map[int]*redisViewerState{}}
	vp := newViewerPool(viewerConfig{
		Servers: servers, Rooms: data.rooms[:1], Run: runTag, RampRate: o.rampRate, JoinTimeout: 10 * time.Second, MaxDialing: 128,
		OnFrame: func(v *viewer, msg []byte, at time.Time) {
			if count, ok := parsePresenceFrame(room, msg); ok {
				obs.state(v).addPresence(at, count)
			}
			obs.onFrame(v, msg, at)
		},
		OnClose: obs.onClose,
	})
	defer vp.closeAll()

	total := o.redisViewers * len(servers)
	ramp := vp.rampTo(ctx, total)
	fmt.Printf("redis: joined %d/%d viewers (%d per server), rejected %d (%s)\n", ramp.Joined, total, o.redisViewers, ramp.Rejected, formatReasons(ramp.Reasons))
	if ramp.Joined != total {
		return fail1("not every viewer joined; refusing to run a partial experiment")
	}

	start := time.Now().Add(500 * time.Millisecond)
	stopSamplers := startMetricSamplers(ctx, client, servers, start)

	fmt.Printf("redis: %d viewers/server x %d servers, %.0f msg/s for %s (start Redis fault now via redis.sh)\n", o.redisViewers, len(servers), o.redisRate, o.duration)
	p := &poster{client: client, servers: servers, rooms: data.rooms[:1], tokens: data.tokens, run: runTag}
	loop := runOpenLoop(runCtx, openLoopConfig{Start: start, Rate: o.redisRate, Measure: o.duration, MaxInFlight: o.maxInFlight, Send: p.send})
	sleepCtx(ctx, o.drain)

	samples, sampleErrs := stopSamplers()
	vp.closeAll()      // 先關 reader（之後不會再 fetchWG.Add），再等補撈；closeAll 可重複呼叫（defer 還會再跑一次）
	obs.fetchWG.Wait() // 進行中的補撈讓它跑完（client 有 timeout）

	obs.mu.Lock()
	var states []*redisViewerState
	for _, v := range vp.joinedViewers() {
		st := obs.states[v.idx]
		if st == nil {
			st = newRedisViewerState(v.idx, v.server) // join 之後一個 frame 都沒收到
		}
		states = append(states, st)
	}
	obs.mu.Unlock()
	report := buildRedisReport(start, o.redisRate, loop, states, servers, samples, sampleErrs)
	report.Summary.ResyncDelayMin = o.redisResyncDelayMin.String()
	report.Summary.ResyncDelayMax = o.redisResyncDelayMax.String()
	// 只有 redis 情境的 poster 直接打每台 server（kill 情境走 nginx，seq → server 對不起來），所以 POST 統計不放在 buildRedisReport
	postStats := buildPostStatsByServer(loop, servers)
	for i := range report.Servers {
		report.Servers[i].Posts = postStats[report.Servers[i].Server]
	}
	rep.Redis = &report
	if ctx.Err() != nil {
		rep.Aborted = true
		rep.Notes = append(rep.Notes, "interrupted: the run was cut short")
		return 130
	}
	return 0
}

func fail1(msg string) int {
	fmt.Fprintln(os.Stderr, "loadtest: "+msg)
	return 1
}

// 摘要的前三行；disconnects 由呼叫端決定怎麼印（kill 情境沒有量測，印 n/a）
func printRedisSummary(w io.Writer, s *redisSummary, scenario, disconnects string) {
	fmt.Fprintf(w, "\nscenario=%s  start=%s  rate=%.0f msg/s  viewers=%d  posts=%d (201: %d)  post status: %s\n",
		scenario, s.Start, s.Rate, s.Viewers, s.Posts, s.PostsCreated, formatReasons(s.PostStatus))
	fmt.Fprintf(w, "resync_delay_min=%s resync_delay_max=%s\n", s.ResyncDelayMin, s.ResyncDelayMax)
	fmt.Fprintf(w, "total: ws_received=%d missing=%d recovered=%d final_missing=%d (older_than_fetch=%d) received_not_201=%d disconnects=%s history_limit=%d\n", s.ReceivedWS, s.Missing, s.Recovered, s.FinalMissing, s.FinalMissingOlder, s.ReceivedNot201, disconnects, s.HistoryLimit)
}

// 給人看的摘要（完整資料在 JSON）
func printRedis(w io.Writer, r *redisReport) {
	printRedisSummary(w, &r.Summary, "redis", strconv.Itoa(r.Summary.Disconnects))
	for _, sv := range r.Servers {
		first := "never"
		if sv.FirstResyncAtS != nil {
			first = fmt.Sprintf("t=%.1fs (%s)", *sv.FirstResyncAtS, sv.FirstResyncAt)
		}
		fmt.Fprintf(w, "%s: viewers=%d missing=%d recovered=%d final_missing=%d resynced_viewers=%d first_resync=%s disconnects=%d metrics: resync_sent+%.0f publish_failed+%.0f pubsub_deaf+%.0f ws_conn=%d..%d\n",
			sv.Server, sv.Viewers, sv.Missing, sv.Recovered, sv.FinalMissing, sv.ResyncViewers, first, sv.Disconnects,
			sv.ResyncSentDelta, sv.PublishFailedDelta, sv.PubSubDeafDelta, sv.ConnectionsMin, sv.ConnectionsMax)
	}
	for _, sv := range r.Servers {
		if p := sv.Presence; p != nil {
			fmt.Fprintf(w, "%s: presence min=%d max=%d first=%d last=%d frames=%d series_5s: %s\n", sv.Server, p.Min, p.Max, p.First, p.Last, p.Frames, formatPresenceSeries(p.Series))
		}
		if ps := sv.Posts; ps != nil {
			fmt.Fprintf(w, "%s: posts sent=%d status: %s 201 latency p50=%.1fms p99=%.1fms max=%.1fms, all latency p50=%.1fms p99=%.1fms max=%.1fms\n", sv.Server, ps.Sent, formatReasons(ps.StatusCounts), ps.Latency201.P50Ms, ps.Latency201.P99Ms, ps.Latency201.MaxMs, ps.LatencyAll.P50Ms, ps.LatencyAll.P99Ms, ps.LatencyAll.MaxMs)
		}
	}
}

// [{0,5},{5,4}] → "0s:5 5s:4"
func formatPresenceSeries(series []presenceBucket) string {
	parts := make([]string, 0, len(series))
	for _, b := range series {
		parts = append(parts, fmt.Sprintf("%ds:%d", b.TS, b.Count))
	}
	return strings.Join(parts, " ")
}
