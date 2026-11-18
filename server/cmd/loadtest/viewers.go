package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 觀眾（WS 連線）：房間是 i % rooms，server 是 (i/rooms) % servers（見 serverIndex，避免房間和 server 綁死），送真的 client 會送的 join frame，等 join_ok。
// 絕大多數連線只讀、丟掉內容（NextReader + io.Copy 到 Discard，不做 JSON parse），省工具自己的 CPU；
// 只有前 Sampled 條連線會 parse frame，拿來量 send-to-receive 延遲與漏收。

type viewerConfig struct {
	Servers     []string // http(s) base URL；WS 網址由它轉成 ws(s)://.../ws
	Rooms       []string // 房間 public id；第 i 條連線進 Rooms[i % len]
	Run         string   // run 標籤，marker 用
	Sampled     int      // 前幾條連線要 parse frame
	RampRate    float64  // 每秒建幾條連線
	JoinTimeout time.Duration
	MaxDialing  int // 同時進行中的 dial + join 數
	// stuck 情境：join_ok 之後不啟動 reader，所以不讀、也不回 ping；連線保持開著，直到 closeAll
	Stuck bool
	// redis 情境：設了就用 readObserved 讀（每個 frame 都交給 OnFrame，斷線交給 OnClose），不走 sampled / recorder 那套。
	// OnFrame 在 reader goroutine 內呼叫，不能阻塞；OnClose 只在非預期斷線時呼叫（closeAll 收尾不算）
	OnFrame func(v *viewer, msg []byte, at time.Time)
	OnClose func(v *viewer, err error, at time.Time)
}

type viewer struct {
	idx     int
	roomIdx int
	server  string // 這條連線連的 server base URL
	sampled bool
	conn    *websocket.Conn
	frames  atomic.Int64 // 收到的 frame 數（每條連線自己一個計數器，避免所有 goroutine 搶同一個）
	alive   atomic.Bool
}

type viewerPool struct {
	cfg    viewerConfig
	dialer *websocket.Dialer

	mu            sync.Mutex
	all           []*viewer // 成功 join 過的連線（alive 旗標表示現在還在不在）
	nextIdx       int
	rejectReasons map[string]int

	wg       sync.WaitGroup // reader goroutines
	closes   atomic.Int64   // join 之後非預期的斷線數
	rejected atomic.Int64   // 沒能進房間的連線數（累計整個 run）
	stopping atomic.Bool
	// 第一條連線 join 成功的時間（unix ns，0 = 還沒有）；stuck 情境用來當時間軸的原點
	firstJoinNs atomic.Int64
	recorder    atomic.Pointer[stepRecorder]
}

func newViewerPool(cfg viewerConfig) *viewerPool {
	return &viewerPool{
		cfg: cfg,
		dialer: &websocket.Dialer{
			HandshakeTimeout: cfg.JoinTimeout,
			ReadBufferSize:   2048,
			WriteBufferSize:  1024,
			NetDialContext:   (&net.Dialer{Timeout: cfg.JoinTimeout}).DialContext,
		},
		rejectReasons: map[string]int{},
	}
}

// 一次 ramp-up 的結果
type rampResult struct {
	Attempted int            `json:"attempted"`
	Joined    int            `json:"joined"`
	Rejected  int            `json:"rejected_joins"`
	Reasons   map[string]int `json:"reject_reasons,omitempty"`
	Seconds   float64        `json:"seconds"`
}

// 把連線補到 target 條（算現在還活著的）。照 RampRate 平滑地建，不一次全部打出去；沒進房間的不重試、記在 rejected
func (p *viewerPool) rampTo(ctx context.Context, target int) rampResult {
	started := time.Now()
	need := target - p.aliveCount()
	res := rampResult{Reasons: map[string]int{}}
	if need <= 0 {
		return res
	}
	interval := time.Duration(float64(time.Second) / p.cfg.RampRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sem := make(chan struct{}, max(p.cfg.MaxDialing, 1))
	var wg sync.WaitGroup
	var mu sync.Mutex

pace:
	for range need {
		select {
		case <-ctx.Done():
			break pace
		case <-ticker.C:
		}
		select {
		case <-ctx.Done():
			break pace
		case sem <- struct{}{}:
		}
		idx := p.takeIdx()
		res.Attempted++
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			reason := p.connect(ctx, idx)
			mu.Lock()
			defer mu.Unlock()
			if reason == "" {
				res.Joined++
				return
			}
			res.Rejected++
			res.Reasons[reason]++
			p.rejected.Add(1)
			p.mu.Lock()
			p.rejectReasons[reason]++
			p.mu.Unlock()
		}()
	}
	wg.Wait()
	res.Seconds = time.Since(started).Seconds()
	return res
}

func (p *viewerPool) takeIdx() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.nextIdx
	p.nextIdx++
	return idx
}

// 對 base 建一條 WS、（token 非空時）先送 auth frame、再送 join，等到 join_ok 為止，順序跟 web client 一樣（先 auth 再 join）。
// 成功回連線與 ""（read deadline 已清掉）；失敗回 nil 與原因（連線已關掉）。connect 與 kill 情境的重連共用
func (p *viewerPool) dialJoin(ctx context.Context, base, room, token string) (*websocket.Conn, string) {
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"

	dialCtx, cancel := context.WithTimeout(ctx, p.cfg.JoinTimeout)
	defer cancel()
	conn, _, err := p.dialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return nil, "dial_error"
	}
	conn.SetReadDeadline(time.Now().Add(p.cfg.JoinTimeout))
	if token != "" {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"auth","payload":{"token":%q}}`, token))); err != nil {
			conn.Close()
			return nil, "write_error"
		}
	}
	join := fmt.Sprintf(`{"type":"join","payload":{"room":%q}}`, room)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(join)); err != nil {
		conn.Close()
		return nil, "write_error"
	}
	// 等 join_ok：中間可能先收到別的 frame（presence 等）；被擋（連線數滿）時 server 已 upgrade 才關，會讀到 close
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil, "join_timeout"
			}
			return nil, "closed_before_join_ok"
		}
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Code string `json:"code"`
			} `json:"payload"`
		}
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		if env.Type == "join_ok" {
			break
		}
		if env.Type == "error" {
			conn.Close()
			return nil, "join_error:" + env.Payload.Code
		}
	}
	conn.SetReadDeadline(time.Time{})
	return conn, ""
}

// 建一條連線、送 join、等 join_ok。成功回 ""，並啟動 reader（Stuck 時不啟動）；失敗回原因（連線已關掉）
func (p *viewerPool) connect(ctx context.Context, idx int) (reason string) {
	base := p.cfg.Servers[serverIndex(idx, len(p.cfg.Rooms), len(p.cfg.Servers))]
	v := &viewer{idx: idx, roomIdx: idx % len(p.cfg.Rooms), sampled: idx < p.cfg.Sampled, server: base}
	conn, reason := p.dialJoin(ctx, base, p.cfg.Rooms[v.roomIdx], "")
	if reason != "" {
		return reason
	}
	v.conn = conn
	v.alive.Store(true)
	p.mu.Lock()
	p.all = append(p.all, v)
	p.mu.Unlock()
	p.firstJoinNs.CompareAndSwap(0, time.Now().UnixNano())
	if !p.cfg.Stuck {
		p.wg.Add(1)
		go p.read(v)
	}
	return ""
}

// 讀取迴圈。一般連線：NextReader + io.Copy 到 Discard，只數 frame；sampled 連線：parse 後交給 recorder
// 連線結束時，如果不是工具自己在收尾，算一次 join 之後的斷線
func (p *viewerPool) read(v *viewer) {
	defer p.wg.Done()
	defer func() {
		v.alive.Store(false)
		if !p.stopping.Load() {
			p.closes.Add(1)
		}
	}()
	if p.cfg.OnFrame != nil {
		p.readObserved(v)
		return
	}
	if v.sampled {
		p.readSampled(v)
		return
	}
	for {
		_, r, err := v.conn.NextReader()
		if err != nil {
			return
		}
		io.Copy(io.Discard, r)
		v.frames.Add(1)
	}
}

var messageTypeNeedle = []byte(`"type":"message"`)

func (p *viewerPool) readSampled(v *viewer) {
	for {
		_, msg, err := v.conn.ReadMessage()
		at := time.Now()
		if err != nil {
			return
		}
		v.frames.Add(1)
		if !bytes.Contains(msg, messageTypeNeedle) {
			continue
		}
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Content string `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(msg, &env) != nil || env.Type != "message" {
			continue
		}
		rec := p.recorder.Load()
		if rec == nil {
			continue
		}
		step, seq, ok := parseMarker(p.cfg.Run, env.Payload.Content)
		if !ok || step != rec.step {
			continue
		}
		rec.record(v.idx, seq, at)
	}
}

// 每個 frame 都交給 cfg.OnFrame；讀取失敗時（不是工具自己收尾）把原因與時間交給 cfg.OnClose，不重連
func (p *viewerPool) readObserved(v *viewer) {
	for {
		_, msg, err := v.conn.ReadMessage()
		at := time.Now()
		if err != nil {
			if !p.stopping.Load() && p.cfg.OnClose != nil {
				p.cfg.OnClose(v, err, at)
			}
			return
		}
		v.frames.Add(1)
		p.cfg.OnFrame(v, msg, at)
	}
}

// 成功 join 過的連線（不論現在還在不在），給 redis 情境列出每條連線用
func (p *viewerPool) joinedViewers() []*viewer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*viewer(nil), p.all...)
}

func (p *viewerPool) aliveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.all {
		if v.alive.Load() {
			n++
		}
	}
	return n
}

// 所有連線累計收到的 frame 數
func (p *viewerPool) totalFrames() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for _, v := range p.all {
		n += v.frames.Load()
	}
	return n
}

// sampled 連線（前 Sampled 條）目前是否還在房間裡，以及它們的房間
func (p *viewerPool) sampleState(samples int) (joined []bool, roomIdx []int) {
	joined = make([]bool, samples)
	roomIdx = make([]int, samples)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range p.all {
		if v.sampled && v.idx < samples {
			joined[v.idx] = v.alive.Load()
			roomIdx[v.idx] = v.roomIdx
		}
	}
	return joined, roomIdx
}

// 到目前為止的拒絕原因（累計）
func (p *viewerPool) reasons() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.rejectReasons))
	for k, v := range p.rejectReasons {
		out[k] = v
	}
	return out
}

// 收尾：標記為預期內的關閉（不算斷線），關掉全部連線並等 reader 結束
func (p *viewerPool) closeAll() {
	p.stopping.Store(true)
	p.mu.Lock()
	conns := make([]*viewer, len(p.all))
	copy(conns, p.all)
	p.mu.Unlock()
	for _, v := range conns {
		v.conn.Close()
	}
	p.wg.Wait()
}

// 把原因表轉成穩定順序的字串，給人看的表格用
func formatReasons(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ",")
}
