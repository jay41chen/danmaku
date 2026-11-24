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
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// -scenario kill：兩台 server 前面有 nginx，跑到一半被外部（deploy/fault/kill.sh）對其中一台下 SIGKILL，觀察：
//  1. 被殺那台的觀眾從斷線到重新 join_ok 要多久（downtime）
//  2. 空窗期間的訊息，有沒有被 join_ok 之後的歷史補撈撈回來
//  3. 房間人數（presence frame 的 count）的走勢：登入的人以 user_id 去重，匿名的每條連線各算一個，
//     所以死掉那台留在 Redis 的匿名 stale 成員（最久 30 秒沒續期才被剔除）會讓人數短暫偏高
//  4. poster 在 kill 當下送出的 POST 拿到什麼（502 只有「server 在處理 request 的途中被殺」才會出現；
//     對已死的容器 IP connect 不會被 RST 而是卡住，所以回應時間與 downtime 都包含這段 connect 卡住的時間，直到 nginx 的 proxy_connect_timeout 才轉去另一台；
//     received_not_201 = 已 commit 但沒拿到 201）
//  5. 每台 server 的 websocket_viewers（member / anonymous）走勢：看被殺那台的觀眾重連後落到哪一台
//
// 觀眾、poster、歷史 GET 全部走 -kill-entry（nginx）；-servers 只用來 seed 與每秒取樣各台的 /metrics（直連）。
// 觀眾（依 idx % 4：0、1 登入，2、3 匿名；見 killLoggedIn）斷線後照 web client 的做法重連：等 equalJitter 退避、重新 auth（登入的）、join，
// join_ok 後立刻撈一次最新歷史（失敗時照 client 重試，見 fetchHistory）。這個工具不重啟被殺的 server。
// 收件紀錄、補撈、outcome、post 時間線、metrics 與報告都沿用 redis 情境（redis.go），這裡只加重連與 presence。

const (
	// 對應 client/src/realtime/connection.ts 的 RECONNECT_BASE_MS / RECONNECT_CAP_MS（兩邊要一起改）
	killReconnectBase = 1 * time.Second
	killReconnectCap  = 32 * time.Second
	// 歷史 GET 失敗的重試：對應 client/src/realtime/connection.ts 的 HISTORY_MAX_ATTEMPTS / HISTORY_RETRY_BASE_MS（兩邊要一起改）
	killHistoryMaxAttempts = 3 // 含第一次
	killHistoryRetryBase   = killReconnectBase
	// 報告的 after_kill 取樣點：第一次斷線後這麼久（略大於觀眾最久的重連退避 32s）
	killSettle = 35 * time.Second
)

// 第 idx 條觀眾是否登入：idx % 4 為 0、1 登入，2、3 匿名。
// 不用嚴格交錯：least_conn 下嚴格交錯會讓匿名觀眾全落在同一台，登入的 presence 去重就測不到
func killLoggedIn(idx int) bool { return idx%4 < 2 }

// n 條觀眾（idx 0..n-1）中登入的個數
func killLoggedInCount(n int) int { return n/4*2 + min(n%4, 2) }

// 登入觀眾 idx 用的 token 在 seeded tokens 裡的位置（只對 killLoggedIn(idx) 為 true 的 idx 有意義）
func killTokenIndex(idx int) int { return idx/4*2 + idx%4 }

// 歷史 GET 失敗後要不要重試：網路錯誤 / timeout / 5xx 重試；4xx、回應解析失敗、自己取消不重試
func historyRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errDecodeHistory) {
		return false
	}
	var se *historyStatusError
	if errors.As(err, &se) {
		return se.Status >= 500
	}
	return true
}

// 可能為 nil 的整數：nil 印成 n/a（沒有量測，不是 0）
func fmtOptInt(v *int) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *v)
}

// 和 client/src/lib/backoff.ts 的 equalJitter 同一個公式：base = min(cap, baseMs * 2^attempt)；wait = base/2 + random * base/2。
// rnd 回 [0, 1)，由呼叫端注入（正式用 rand.Float64，測試用固定值）。
// 用 float64 毫秒算，跟 TS 一樣：attempt 很大時 2^attempt 不會像整數位移那樣溢位
func equalJitter(attempt int, base, limit time.Duration, rnd func() float64) time.Duration {
	baseMs := float64(base) / float64(time.Millisecond)
	capMs := float64(limit) / float64(time.Millisecond)
	b := math.Min(capMs, baseMs*math.Pow(2, float64(attempt)))
	return time.Duration((b/2 + rnd()*(b/2)) * float64(time.Millisecond))
}

// 解析 presence frame：room 相符且 count 是整數才回 ok。其他 frame 一律 ok=false
func parsePresenceFrame(room string, msg []byte) (count int, ok bool) {
	if !bytes.Contains(msg, presenceTypeNeedle) {
		return 0, false
	}
	var env struct {
		Type    string `json:"type"`
		Payload struct {
			Room  string `json:"room"`
			Count *int   `json:"count"`
		} `json:"payload"`
	}
	if json.Unmarshal(msg, &env) != nil || env.Type != "presence" || env.Payload.Room != room || env.Payload.Count == nil {
		return 0, false
	}
	return *env.Payload.Count, true
}

var presenceTypeNeedle = []byte(`"type":"presence"`)

type presenceFrame struct {
	At    time.Time
	Count int
}

// 一條 kill 觀眾的紀錄（連線層）；收件與補撈記在 redisObserver 的 redisViewerState（同一個 idx）
type killViewerState struct {
	idx      int
	loggedIn bool
	token    string // 登入的才有；絕不印出
	v        *viewer

	mu          sync.Mutex
	joins       []time.Time // 每次 join_ok
	disconnects []time.Time // 每次 join 之後的非預期斷線
	attempts    int         // 第一次連線之後又發起的連線次數（含失敗的）
	presence    []presenceFrame

	// join_ok 後歷史 GET 的次數（含重試）、其中失敗的次數、以及放棄的次數
	// gave_up = 最後一次嘗試仍失敗（次數用完，或不可重試的錯誤如 4xx）；run 的 ctx 結束造成的 canceled / deadline exceeded 與被新一輪取代的不算
	historyAttempts int
	historyFailed   int
	historyGaveUp   int
	// 歷史 fetch 的世代（對應 client 的 historyFetchGen）：每次 join_ok 開新一輪就加一，舊一輪的重試鏈發現世代變了就靜默停止
	historyGen int
}

// 開新一輪歷史 fetch：世代加一並回傳，同時讓舊的重試鏈失效
func (k *killViewerState) nextHistoryGen() int {
	k.mu.Lock()
	k.historyGen++
	g := k.historyGen
	k.mu.Unlock()
	return g
}

// gen 這一輪是否已被較新的一輪取代
func (k *killViewerState) historySuperseded(gen int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.historyGen != gen
}

func (k *killViewerState) addJoin(at time.Time) {
	k.mu.Lock()
	k.joins = append(k.joins, at)
	k.mu.Unlock()
}

func (k *killViewerState) addDisconnect(at time.Time) {
	k.mu.Lock()
	k.disconnects = append(k.disconnects, at)
	k.mu.Unlock()
}

func (k *killViewerState) addHistoryAttempt(failed bool) {
	k.mu.Lock()
	k.historyAttempts++
	if failed {
		k.historyFailed++
	}
	k.mu.Unlock()
}

func (k *killViewerState) addHistoryGaveUp() {
	k.mu.Lock()
	k.historyGaveUp++
	k.mu.Unlock()
}

func (k *killViewerState) addPresence(at time.Time, count int) {
	k.mu.Lock()
	k.presence = append(k.presence, presenceFrame{At: at, Count: count})
	k.mu.Unlock()
}

// 每次斷線到「下一次 join_ok」的時間；之後一直沒 join 回來的斷線（run 結束時還沒重連上）算進 pending，不進 downtimes。
// 兩個 slice 都要依時間遞增；join 一定在對應的 disconnect 之前，所以找「晚於這次斷線的第一個 join」
func computeDowntimes(joins, disconnects []time.Time) (downtimes []time.Duration, pending int) {
	for _, d := range disconnects {
		i := slices.IndexFunc(joins, func(j time.Time) bool { return j.After(d) })
		if i < 0 {
			pending++
			continue
		}
		downtimes = append(downtimes, joins[i].Sub(d))
	}
	return downtimes, pending
}

// 某條觀眾「在房間裡」的時間區間。To 為零值 = 到 run 結束還在
type joinedSpan struct{ From, To time.Time }

// 由 join / disconnect 時間組出區間：第 i 個 join 到「晚於它的第一個斷線」
func joinedSpans(joins, disconnects []time.Time) []joinedSpan {
	var out []joinedSpan
	for _, j := range joins {
		sp := joinedSpan{From: j}
		if i := slices.IndexFunc(disconnects, func(d time.Time) bool { return d.After(j) }); i >= 0 {
			sp.To = disconnects[i]
		}
		out = append(out, sp)
	}
	return out
}

// 工具自己的狀態推出來的「現在應該有幾個人在房間」：所有觀眾的區間裡，涵蓋 t 的個數。
// 登入的觀眾各用不同的 user，匿名的各算一條連線，所以人數 = 在房間裡的觀眾數
func expectedJoined(spans [][]joinedSpan, t time.Time) int {
	n := 0
	for _, list := range spans {
		for _, sp := range list {
			if !sp.From.After(t) && (sp.To.IsZero() || t.Before(sp.To)) {
				n++
				break
			}
		}
	}
	return n
}

// ---- 報告 ----

type killPresenceJSON struct {
	AtS      float64 `json:"at_s"`
	Count    int     `json:"count"`
	Expected int     `json:"expected"`
}

type killDowntimeJSON struct {
	DisconnectAtS float64  `json:"disconnect_at_s"`
	JoinOKAtS     *float64 `json:"join_ok_at_s,omitempty"` // nil = 到 run 結束都沒重連上
	DowntimeS     *float64 `json:"downtime_s,omitempty"`
}

type killViewerResult struct {
	Viewer            int                `json:"viewer"`
	LoggedIn          bool               `json:"logged_in"`
	Disconnects       []killDowntimeJSON `json:"disconnects,omitempty"`
	ReconnectAttempts int                `json:"reconnect_attempts"`
	// join_ok 後歷史 GET 的次數（含重試）、失敗次數、放棄次數（最後一次嘗試仍失敗：次數用完或不可重試的錯誤如 4xx；ctx 結束與被取代的不算）；attempts > 呼叫次數代表有重試發生
	HistoryAttempts int                `json:"history_attempts"`
	HistoryFailed   int                `json:"history_failed_attempts"`
	HistoryGaveUp   int                `json:"history_gave_up"`
	Missing         int                `json:"missing"`
	Recovered       int                `json:"recovered"`
	FinalMissing    int                `json:"final_missing"`
	Presence        []killPresenceJSON `json:"presence_frames,omitempty"`
}

type killPresenceSummary struct {
	Frames int `json:"frames"`
	// 第一次斷線（= kill 的觀察時間點）之後收到的最大 count，以及當時工具認為的人數
	MaxAfterKill        *int     `json:"max_count_after_kill,omitempty"`
	ExpectedAtMaxAfter  *int     `json:"expected_at_max_count_after_kill,omitempty"`
	MaxExcessAfterKill  *int     `json:"max_count_minus_expected_after_kill,omitempty"`
	MinDeficitAfterKill *int     `json:"min_count_minus_expected_after_kill,omitempty"`
	LastDiffAtS         *float64 `json:"last_differed_at_s,omitempty"`         // 最後一次 count != expected 的 frame 時間（整段 run，nil = 從沒不同）
	LastDiffAfterKillS  *float64 `json:"last_differed_after_kill_s,omitempty"` // 最後一次不同，距第一次斷線幾秒
	FramesDifferedTotal int      `json:"frames_differed"`
}

type killSummary struct {
	Viewers            int                 `json:"viewers"`
	LoggedIn           int                 `json:"viewers_logged_in"`
	Anonymous          int                 `json:"viewers_anonymous"`
	Disconnected       int                 `json:"viewers_disconnected"`
	DisconnectedLogged int                 `json:"viewers_disconnected_logged_in"`
	DisconnectedAnon   int                 `json:"viewers_disconnected_anonymous"`
	FirstDisconnectAtS *float64            `json:"first_disconnect_at_s,omitempty"`
	Disconnects        int                 `json:"disconnect_events"`
	Rejoined           int                 `json:"rejoined_events"`
	NeverRejoined      int                 `json:"never_rejoined_events"`
	ReconnectAttempts  int                 `json:"reconnect_attempts"`
	HistoryAttempts    int                 `json:"history_attempts"`
	HistoryFailed      int                 `json:"history_failed_attempts"`
	HistoryGaveUp      int                 `json:"history_gave_up"`
	DowntimeMinS       *float64            `json:"downtime_min_s,omitempty"`
	DowntimeMedianS    *float64            `json:"downtime_median_s,omitempty"`
	DowntimeMaxS       *float64            `json:"downtime_max_s,omitempty"`
	Missing            int                 `json:"missing"`
	Recovered          int                 `json:"recovered"`
	FinalMissing       int                 `json:"final_missing"`
	PostStatus         map[string]int      `json:"post_status_total"`
	Post502            int                 `json:"post_502"`
	ReceivedNot201     int                 `json:"received_not_201"`
	Presence           killPresenceSummary `json:"presence"`
	Note               string              `json:"note"`
}

// 某台 server 在某個時間點的 websocket_viewers
type wsViewersSnap struct {
	AtS       float64 `json:"at_s"`
	Member    int     `json:"member"`
	Anonymous int     `json:"anonymous"`
}

// redisServerSummary 加上 kill 情境專用欄位。
// 觀眾都連 nginx（入口），不是直連各台，所以 redis 那組「這台有幾個觀眾 / 收到多少 / 漏多少 / 斷線幾次」在這裡沒有量測：
// 用同名的指標欄位蓋掉內嵌的 int（nil + omitempty = JSON 省略，印出來是 n/a），避免 0 被誤讀成「真的是 0」
type killServerSummary struct {
	redisServerSummary
	Viewers       *int `json:"viewers,omitempty"`
	ReceivedWS    *int `json:"received_ws,omitempty"`
	Missing       *int `json:"missing,omitempty"`
	Recovered     *int `json:"recovered,omitempty"`
	FinalMissing  *int `json:"final_missing,omitempty"`
	ResyncViewers *int `json:"viewers_resynced,omitempty"`
	Disconnects   *int `json:"disconnects,omitempty"`

	// 取自 /metrics 的 websocket_viewers（逐秒的完整時間序列在 base.metric_samples）。
	// before_kill = 第一次斷線前最後一筆；after_kill = 第一次斷線後 killSettle 的第一筆；end = 最後一筆成功的取樣（被殺的那台是被殺前的最後一筆）
	WSViewersBeforeKill *wsViewersSnap `json:"ws_viewers_before_kill,omitempty"`
	WSViewersAfterKill  *wsViewersSnap `json:"ws_viewers_after_kill,omitempty"`
	WSViewersEnd        *wsViewersSnap `json:"ws_viewers_end,omitempty"`
}

// redisSummary 的 kill 版：disconnects 這個 redis 情境才有量測的欄位在 kill 沒有（斷線記在 kill 自己的 summary），省略而不是印 0
type killBaseSummary struct {
	redisSummary
	Disconnects *int `json:"disconnects,omitempty"`
}

// 收件、補撈、post 時間線與 /metrics：跟 redis 情境同一份格式（viewer 的 server 欄位 = 入口），但沒量測的欄位不輸出 0
type killBase struct {
	Summary  killBaseSummary     `json:"summary"`
	Servers  []killServerSummary `json:"servers"`
	Viewers  []redisViewerResult `json:"viewers"`
	Timeline []postSecond        `json:"post_timeline"`
	Metrics  []redisMetricSample `json:"metric_samples"`
}

type killReport struct {
	Summary killSummary        `json:"summary"`
	Viewers []killViewerResult `json:"viewers"`
	Base    killBase           `json:"base"`
}

// 某台 server 的 websocket_viewers 快照。samples 要依時間排序；killAt 零值 = 沒有人斷線（沒有 before / after）
func viewerSnapshots(samples []redisMetricSample, server string, killAt time.Time) (before, after, end *wsViewersSnap) {
	snap := func(m *redisMetricSample) *wsViewersSnap {
		return &wsViewersSnap{AtS: m.TS, Member: m.ViewersMember, Anonymous: m.ViewersAnonymous}
	}
	for i := range samples {
		m := &samples[i]
		if m.Server != server {
			continue
		}
		end = snap(m)
		if killAt.IsZero() {
			continue
		}
		if m.at.Before(killAt) {
			before = snap(m)
		} else if after == nil && !m.at.Before(killAt.Add(killSettle)) {
			after = snap(m)
		}
	}
	return before, after, end
}

// 彙整 presence：frames 是所有觀眾收到的 frame（任意順序），expected 是工具自己算的人數，killAt 零值 = 沒有人斷線。純計算
func summarizePresence(frames []presenceFrame, expected func(time.Time) int, killAt, start time.Time) killPresenceSummary {
	sum := killPresenceSummary{Frames: len(frames)}
	var lastDiff time.Time
	for _, f := range frames {
		exp := expected(f.At)
		diff := f.Count - exp
		if diff != 0 {
			sum.FramesDifferedTotal++
			if f.At.After(lastDiff) {
				lastDiff = f.At
			}
		}
		if killAt.IsZero() || f.At.Before(killAt) {
			continue
		}
		if sum.MaxAfterKill == nil || f.Count > *sum.MaxAfterKill {
			c, e := f.Count, exp
			sum.MaxAfterKill, sum.ExpectedAtMaxAfter = &c, &e
		}
		if sum.MaxExcessAfterKill == nil || diff > *sum.MaxExcessAfterKill {
			d := diff
			sum.MaxExcessAfterKill = &d
		}
		if sum.MinDeficitAfterKill == nil || diff < *sum.MinDeficitAfterKill {
			d := diff
			sum.MinDeficitAfterKill = &d
		}
	}
	if !lastDiff.IsZero() {
		s := relS(lastDiff, start)
		sum.LastDiffAtS = &s
		if !killAt.IsZero() {
			a := lastDiff.Sub(killAt).Seconds()
			sum.LastDiffAfterKillS = &a
		}
	}
	return sum
}

func fptr(v float64) *float64 { return &v }

// 組 kill 報告：純計算。base 是 buildRedisReport 的結果（收件、補撈、post 時間線、metrics）
func buildKillReport(start time.Time, base redisReport, ks []*killViewerState) killReport {
	rep := killReport{Base: killBase{Summary: killBaseSummary{redisSummary: base.Summary}, Viewers: base.Viewers, Timeline: base.Timeline, Metrics: base.Metrics}}
	slices.SortFunc(ks, func(a, b *killViewerState) int { return a.idx - b.idx })
	sum := killSummary{Viewers: len(ks), PostStatus: base.Summary.PostStatus, Post502: base.Summary.PostStatus["502"], ReceivedNot201: base.Summary.ReceivedNot201,
		Note: "downtime = disconnect to the next join_ok (includes the client-style equalJitter backoff 1s..32s, any failed attempts, and the connect hang to the dead container IP: a killed container sends no RST, so connects hang until nginx proxy_connect_timeout; 502 only appears for requests that were in flight on the killed server, not for new connections to the dead IP); per-server fields that the kill scenario does not measure (viewers / received / missing / disconnects per direct server, because viewers connect through nginx) are omitted from the JSON and printed as n/a, never 0; per-server websocket_viewers snapshots come from the /metrics samples; missing/recovered/final_missing as in the redis scenario (fetches right after join_ok count as recovery); presence expected = number of viewers currently joined by the tool's own state, but the server only pushes a count every 5s and syncs membership asynchronously, so short differences around join/disconnect are normal; first_disconnect is the tool's observation of the kill (T_kill from kill.sh is the reference); history_gave_up counts history GET chains whose last attempt still failed (attempts exhausted or a non-retryable error such as 4xx), not chains ended by the run's context being canceled or superseded by a newer join_ok"}

	spans := make([][]joinedSpan, len(ks))
	var killAt time.Time
	var downtimes []time.Duration
	var frames []presenceFrame
	// 已有的 created 集合用 base 的 viewer 結果（computeOutcome 已算好）；這裡只取每條的 missing 等數字
	byIdx := map[int]redisViewerResult{}
	for _, v := range base.Viewers {
		byIdx[v.Viewer] = v
	}
	for i, st := range ks {
		st.mu.Lock()
		joins, disc, pres, attempts := slices.Clone(st.joins), slices.Clone(st.disconnects), slices.Clone(st.presence), st.attempts
		hAttempts, hFailed, hGaveUp := st.historyAttempts, st.historyFailed, st.historyGaveUp
		st.mu.Unlock()
		spans[i] = joinedSpans(joins, disc)
		dts, pending := computeDowntimes(joins, disc)
		downtimes = append(downtimes, dts...)
		frames = append(frames, pres...)

		vr := killViewerResult{Viewer: st.idx, LoggedIn: st.loggedIn, ReconnectAttempts: attempts, HistoryAttempts: hAttempts, HistoryFailed: hFailed, HistoryGaveUp: hGaveUp}
		if r, ok := byIdx[st.idx]; ok {
			vr.Missing, vr.Recovered, vr.FinalMissing = r.Missing, r.Recovered, r.FinalMissing
		}
		for _, d := range disc {
			e := killDowntimeJSON{DisconnectAtS: relS(d, start)}
			if j := slices.IndexFunc(joins, func(j time.Time) bool { return j.After(d) }); j >= 0 {
				e.JoinOKAtS = fptr(relS(joins[j], start))
				e.DowntimeS = fptr(joins[j].Sub(d).Seconds())
			}
			vr.Disconnects = append(vr.Disconnects, e)
			if killAt.IsZero() || d.Before(killAt) {
				killAt = d
			}
		}
		if st.loggedIn {
			sum.LoggedIn++
		} else {
			sum.Anonymous++
		}
		if len(disc) > 0 {
			sum.Disconnected++
			if st.loggedIn {
				sum.DisconnectedLogged++
			} else {
				sum.DisconnectedAnon++
			}
		}
		sum.Disconnects += len(disc)
		sum.NeverRejoined += pending
		sum.Rejoined += len(dts)
		sum.ReconnectAttempts += attempts
		sum.HistoryAttempts += hAttempts
		sum.HistoryFailed += hFailed
		sum.HistoryGaveUp += hGaveUp
		sum.Missing += vr.Missing
		sum.Recovered += vr.Recovered
		sum.FinalMissing += vr.FinalMissing
		rep.Viewers = append(rep.Viewers, vr)
	}
	slices.SortFunc(frames, func(a, b presenceFrame) int { return a.At.Compare(b.At) })
	expected := func(t time.Time) int { return expectedJoined(spans, t) }
	for i, st := range ks {
		st.mu.Lock()
		for _, f := range st.presence {
			rep.Viewers[i].Presence = append(rep.Viewers[i].Presence, killPresenceJSON{AtS: relS(f.At, start), Count: f.Count, Expected: expected(f.At)})
		}
		st.mu.Unlock()
	}
	if !killAt.IsZero() {
		sum.FirstDisconnectAtS = fptr(relS(killAt, start))
	}
	if len(downtimes) > 0 {
		s := summarize(downtimes)
		sum.DowntimeMinS = fptr(slices.Min(downtimes).Seconds())
		sum.DowntimeMedianS = fptr(s.P50Ms / 1000)
		sum.DowntimeMaxS = fptr(s.MaxMs / 1000)
	}
	sum.Presence = summarizePresence(frames, expected, killAt, start)
	for _, sv := range base.Servers {
		ks := killServerSummary{redisServerSummary: sv}
		ks.WSViewersBeforeKill, ks.WSViewersAfterKill, ks.WSViewersEnd = viewerSnapshots(base.Metrics, sv.Server, killAt)
		rep.Base.Servers = append(rep.Base.Servers, ks)
	}
	rep.Summary = sum
	return rep
}

// ---- 執行 ----

type killRun struct {
	pool  *viewerPool
	obs   *redisObserver
	entry string
	room  string
	// 退避：nil 時用 equalJitter + rand.Float64；測試可換
	backoff func(attempt int) time.Duration
	// 歷史 GET 重試前的等待（attempt 從 0 起算）：nil 時用 equalJitter(killHistoryRetryBase)；測試可換
	historyBackoff func(attempt int) time.Duration
}

func (r *killRun) wait(attempt int) time.Duration {
	if r.backoff != nil {
		return r.backoff(attempt)
	}
	return equalJitter(attempt, killReconnectBase, killReconnectCap, rand.Float64)
}

// 每個 frame：先記 presence，再交給 redisObserver 處理訊息 marker 與 resync（延遲補撈）
func (r *killRun) onFrame(ks *killViewerState, msg []byte, at time.Time) {
	if count, ok := parsePresenceFrame(r.room, msg); ok {
		ks.addPresence(at, count)
	}
	r.obs.onFrame(ks.v, msg, at)
}

func (r *killRun) historyWait(attempt int) time.Duration {
	if r.historyBackoff != nil {
		return r.historyBackoff(attempt)
	}
	return equalJitter(attempt, killHistoryRetryBase, killReconnectCap, rand.Float64)
}

// 撈一次最新歷史，失敗時照 web client 的做法重試：最多 killHistoryMaxAttempts 次（含第一次），每次之間等 equalJitter 退避；
// 只有網路錯誤 / timeout / 5xx 才重試（historyRetryable）。每次呼叫都記進 ks 的 history 計數。
// 回傳最後一次的結果。gave_up = 最後一次嘗試仍失敗（次數用完，或不可重試的錯誤如 4xx）；
// run 的 ctx 結束（canceled / deadline exceeded）或 gen 被新一輪取代時靜默結束，不算 gave_up。
// 跟 client 的 historyFetchGen 一樣：舊一輪不會再排下一次重試
func (r *killRun) fetchHistory(ks *killViewerState, gen int) ([]int, error) {
	var seqs []int
	var err error
	for attempt := range killHistoryMaxAttempts {
		seqs, err = r.obs.fetch(r.entry)
		ks.addHistoryAttempt(err != nil)
		if err == nil {
			return seqs, nil
		}
		if r.obs.ctx.Err() != nil || ks.historySuperseded(gen) {
			return seqs, err
		}
		if !historyRetryable(err) || attempt == killHistoryMaxAttempts-1 {
			break
		}
		t := time.NewTimer(r.historyWait(attempt))
		select {
		case <-r.obs.ctx.Done():
			t.Stop()
			return seqs, err
		case <-t.C:
		}
		if ks.historySuperseded(gen) {
			return seqs, err
		}
	}
	ks.addHistoryGaveUp()
	return seqs, err
}

// join_ok 之後立刻撈一次最新歷史（跟 web client 一樣，含失敗重試，見 fetchHistory），記成一次 fetch。放在自己的 goroutine，不擋 reader；
// fetchWG.Add 在呼叫端同步做（viewer goroutine 全部結束後才 Wait）
func (r *killRun) fetchAfterJoin(ks *killViewerState) {
	st := r.obs.state(ks.v)
	gen := ks.nextHistoryGen()
	r.obs.fetchWG.Add(1)
	go func() {
		defer r.obs.fetchWG.Done()
		seqs, err := r.fetchHistory(ks, gen)
		st.addFetch(time.Now(), seqs, err)
	}()
}

// 一條觀眾的生命週期：連線 → join_ok → 讀到斷線 → 等退避 → 重連，直到 ctx 取消。
// 退避次數跟 client 一樣：每次斷線或連線 / join 失敗加一，join_ok 才歸零。firstJoined 在第一次 join_ok 時呼叫一次
func (r *killRun) runViewer(ctx context.Context, ks *killViewerState, firstJoined func()) {
	attempt := 0
	first := true
	for ctx.Err() == nil {
		if !first {
			ks.mu.Lock()
			ks.attempts++
			ks.mu.Unlock()
		}
		first = false
		conn, reason := r.pool.dialJoin(ctx, r.entry, r.room, ks.token)
		if reason == "" {
			attempt = 0
			ks.addJoin(time.Now())
			firstJoined()
			r.fetchAfterJoin(ks)
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			at, _ := r.readLoop(ks, conn)
			stop()
			conn.Close()
			if ctx.Err() != nil {
				return
			}
			ks.addDisconnect(at)
		}
		t := time.NewTimer(r.wait(attempt))
		attempt++
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// 讀到失敗為止，回傳失敗的時間與原因
func (r *killRun) readLoop(ks *killViewerState, conn *websocket.Conn) (time.Time, error) {
	for {
		_, msg, err := conn.ReadMessage()
		at := time.Now()
		if err != nil {
			return at, err
		}
		r.onFrame(ks, msg, at)
	}
}

// 跑 kill 情境。回傳 exit code（0 / 1 / 130）；資料清理由呼叫端 main.run 的 defer 負責
func runKill(ctx context.Context, o *options, client *http.Client, servers []string, entry string, data *seeded, runTag string, rep *runReport) int {
	room := data.rooms[0]
	loggedIn := killLoggedInCount(o.killViewers)
	if loggedIn > len(data.tokens) {
		return fail1(fmt.Sprintf("need %d logged-in viewers but only %d seeded users (use -users)", loggedIn, len(data.tokens)))
	}
	if err := preflight(ctx, client, []string{entry}, room); err != nil {
		return fail1("preflight (entry): " + err.Error())
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	viewerCtx, cancelViewers := context.WithCancel(ctx)
	defer cancelViewers()
	obs := &redisObserver{ctx: runCtx, client: client, run: runTag, room: room, resyncDelayMin: o.redisResyncDelayMin, resyncDelayMax: o.redisResyncDelayMax, states: map[int]*redisViewerState{}}
	vp := newViewerPool(viewerConfig{Servers: []string{entry}, Rooms: []string{room}, Run: runTag, RampRate: o.rampRate, JoinTimeout: 10 * time.Second, MaxDialing: 128})
	kr := &killRun{pool: vp, obs: obs, entry: entry, room: room}

	var states []*killViewerState
	var viewerWG, readyWG sync.WaitGroup
	for i := range o.killViewers {
		ks := &killViewerState{idx: i, loggedIn: killLoggedIn(i), v: &viewer{idx: i, server: entry}}
		if ks.loggedIn {
			ks.token = data.tokens[killTokenIndex(i)]
		}
		states = append(states, ks)
	}
	interval := time.Duration(float64(time.Second) / o.rampRate)
	for i, ks := range states {
		viewerWG.Add(1)
		readyWG.Add(1)
		var once sync.Once
		go func() {
			defer viewerWG.Done()
			// 照 -ramp-rate 錯開起跑，不一次全部打出去
			t := time.NewTimer(time.Duration(i) * interval)
			select {
			case <-viewerCtx.Done():
				t.Stop()
				once.Do(readyWG.Done)
				return
			case <-t.C:
			}
			kr.runViewer(viewerCtx, ks, func() { once.Do(readyWG.Done) })
		}()
	}
	ready := make(chan struct{})
	go func() { readyWG.Wait(); close(ready) }()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		cancelViewers()
		viewerWG.Wait()
		obs.fetchWG.Wait()
		return fail1("not every viewer joined within 30s; refusing to run a partial experiment")
	case <-ctx.Done():
		cancelViewers()
		viewerWG.Wait()
		obs.fetchWG.Wait()
		return 130
	}
	fmt.Printf("kill: joined %d/%d viewers via %s (%d logged in, %d anonymous)\n", o.killViewers, o.killViewers, entry, loggedIn, o.killViewers-loggedIn)

	start := time.Now().Add(500 * time.Millisecond)
	stopSamplers := startMetricSamplers(ctx, client, servers, start)

	fmt.Printf("kill: %d viewers, %.0f msg/s for %s through %s (kill a server now via kill.sh)\n", o.killViewers, o.killRate, o.duration, entry)
	p := &poster{client: client, servers: []string{entry}, rooms: data.rooms[:1], tokens: data.tokens, run: runTag}
	loop := runOpenLoop(runCtx, openLoopConfig{Start: start, Rate: o.killRate, Measure: o.duration, MaxInFlight: o.maxInFlight, Send: p.send})
	sleepCtx(ctx, o.drain)

	samples, sampleErrs := stopSamplers()
	cancelViewers() // 先停所有 viewer goroutine（之後不會再 fetchWG.Add），再等補撈
	viewerWG.Wait()
	obs.fetchWG.Wait()

	obs.mu.Lock()
	var rstates []*redisViewerState
	for _, ks := range states {
		st := obs.states[ks.idx]
		if st == nil {
			st = newRedisViewerState(ks.idx, entry)
		}
		rstates = append(rstates, st)
	}
	obs.mu.Unlock()
	base := buildRedisReport(start, o.killRate, loop, rstates, servers, samples, sampleErrs)
	base.Summary.ResyncDelayMin = o.redisResyncDelayMin.String()
	base.Summary.ResyncDelayMax = o.redisResyncDelayMax.String()
	report := buildKillReport(start, base, states)
	rep.Kill = &report
	if ctx.Err() != nil {
		rep.Aborted = true
		rep.Notes = append(rep.Notes, "interrupted: the run was cut short")
		return 130
	}
	return 0
}

// 給人看的摘要（完整資料在 JSON）
func printKill(w io.Writer, r *killReport) {
	s := r.Summary
	opt := func(v *float64) string {
		if v == nil {
			return "n/a"
		}
		return fmt.Sprintf("%.1fs", *v)
	}
	printRedisSummary(w, &r.Base.Summary.redisSummary, "kill", "n/a")
	for _, sv := range r.Base.Servers {
		fmt.Fprintf(w, "%s: viewers=%s missing=%s recovered=%s final_missing=%s resynced_viewers=%s disconnects=%s (not measured per direct server: viewers connect via nginx) metrics: resync_sent+%.0f publish_failed+%.0f pubsub_deaf+%.0f ws_conn=%d..%d\n",
			sv.Server, fmtOptInt(sv.Viewers), fmtOptInt(sv.Missing), fmtOptInt(sv.Recovered), fmtOptInt(sv.FinalMissing), fmtOptInt(sv.ResyncViewers), fmtOptInt(sv.Disconnects),
			sv.ResyncSentDelta, sv.PublishFailedDelta, sv.PubSubDeafDelta, sv.ConnectionsMin, sv.ConnectionsMax)
		fmt.Fprintf(w, "%s: websocket_viewers (member/anonymous): before_kill=%s after_kill=%s end=%s\n",
			sv.Server, fmtSnap(sv.WSViewersBeforeKill), fmtSnap(sv.WSViewersAfterKill), fmtSnap(sv.WSViewersEnd))
	}
	fmt.Fprintf(w, "viewers: %d (logged_in=%d anonymous=%d); disconnected viewers=%d (logged_in=%d anonymous=%d); first disconnect at t=%s\n",
		s.Viewers, s.LoggedIn, s.Anonymous, s.Disconnected, s.DisconnectedLogged, s.DisconnectedAnon, opt(s.FirstDisconnectAtS))
	fmt.Fprintf(w, "downtime (disconnect -> join_ok): min=%s median=%s max=%s; rejoined=%d never_rejoined=%d reconnect_attempts=%d\n",
		opt(s.DowntimeMinS), opt(s.DowntimeMedianS), opt(s.DowntimeMaxS), s.Rejoined, s.NeverRejoined, s.ReconnectAttempts)
	fmt.Fprintf(w, "note: downtime includes the connect hang to the dead container IP (no RST) until nginx proxy_connect_timeout; 502 only appears for requests in flight on the killed server\n")
	fmt.Fprintf(w, "history GET after join_ok: attempts=%d failed=%d gave_up=%d (retries happened when attempts > fetches)\n", s.HistoryAttempts, s.HistoryFailed, s.HistoryGaveUp)
	fmt.Fprintf(w, "history: missing=%d recovered=%d final_missing=%d; posts: %s (502=%d) received_not_201=%d\n",
		s.Missing, s.Recovered, s.FinalMissing, formatReasons(s.PostStatus), s.Post502, s.ReceivedNot201)
	p := s.Presence
	fmt.Fprintf(w, "presence: frames=%d; after kill: max count=%s (expected then %s), count-expected max=%s min=%s; last differed from expected at t=%s (%s after first disconnect); frames differed=%d\n",
		p.Frames, fmtOptInt(p.MaxAfterKill), fmtOptInt(p.ExpectedAtMaxAfter), fmtOptInt(p.MaxExcessAfterKill), fmtOptInt(p.MinDeficitAfterKill),
		opt(p.LastDiffAtS), opt(p.LastDiffAfterKillS), p.FramesDifferedTotal)
}

// wsViewersSnap 的一行文字；nil（沒有那個時間點的取樣）印 n/a
func fmtSnap(v *wsViewersSnap) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d@t=%.0fs", v.Member, v.Anonymous, v.AtS)
}
