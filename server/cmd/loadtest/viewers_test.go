package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 假的 WS server：收到 join 後回 join_ok，之後只在 push 被呼叫時對所有連線送 frame；
// 超過 cap 的連線 upgrade 之後直接關掉（跟真的 server 滿了的行為一樣）
type fakeWSServer struct {
	srv   *httptest.Server
	cap   int
	count atomic.Int64
	mu    sync.Mutex
	conns []*websocket.Conn
	joins []string // 收到的 join frame
}

func newFakeWSServer(t *testing.T, cap int) *fakeWSServer {
	f := &fakeWSServer{cap: cap}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if int(f.count.Add(1)) > f.cap {
			c.Close()
			return
		}
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		// 先寫 join_ok、再登記進 conns：之後只有 push 會寫這條連線，gorilla 不允許兩個 writer 同時寫
		c.WriteMessage(websocket.TextMessage, []byte(`{"type":"join_ok","payload":{"room":"r"}}`))
		f.mu.Lock()
		f.joins = append(f.joins, string(msg))
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		for { // 讀到連線結束
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWSServer) push(frame string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.WriteMessage(websocket.TextMessage, []byte(frame))
	}
}

func TestViewerPoolJoinsCountsAndRecords(t *testing.T) {
	f := newFakeWSServer(t, 4)
	pool := newViewerPool(viewerConfig{
		Servers: []string{f.srv.URL}, Rooms: []string{"roomAAAAAAA"}, Run: "run1", Sampled: 2,
		RampRate: 1000, JoinTimeout: 2 * time.Second, MaxDialing: 8,
	})
	defer pool.closeAll()

	// 要 6 條，server 只收 4 條：兩條 upgrade 後被關 → rejected，不是 join 後斷線
	res := pool.rampTo(context.Background(), 6)
	if res.Attempted != 6 || res.Joined != 4 || res.Rejected != 2 || res.Reasons["closed_before_join_ok"] != 2 {
		t.Fatalf("ramp = %+v, want 6 attempted / 4 joined / 2 rejected closed_before_join_ok", res)
	}
	if pool.aliveCount() != 4 || pool.closes.Load() != 0 {
		t.Errorf("alive = %d closes = %d, want 4 and 0", pool.aliveCount(), pool.closes.Load())
	}
	// 真的 client 送的 join frame 格式
	f.mu.Lock()
	var in struct {
		Type    string `json:"type"`
		Payload struct {
			Room string `json:"room"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(f.joins[0]), &in); err != nil || in.Type != "join" || in.Payload.Room != "roomAAAAAAA" {
		t.Errorf("join frame = %q", f.joins[0])
	}
	f.mu.Unlock()

	// 假 server 在回 join_ok 之後才登記連線：等 4 條都登記好再推，不然會漏推
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		n := len(f.conns)
		f.mu.Unlock()
		if n == 4 {
			break
		}
	}
	// 推一則帶 marker 的訊息與一則 presence：sampled 連線要記到 marker，其他連線只數 frame
	start := time.Now()
	rec := newStepRecorder(0, start, 2, 10)
	pool.recorder.Store(rec)
	f.push(`{"type":"message","payload":{"id":1,"content":"` + makeMarker("run1", 0, 7) + `"}}`)
	f.push(`{"type":"message","payload":{"id":2,"content":"` + makeMarker("run1", 5, 8) + `"}}`) // 別的 step：忽略
	f.push(`{"type":"presence","payload":{"room":"r","count":4}}`)

	deadline := time.Now().Add(2 * time.Second)
	for pool.totalFrames() < 12 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := pool.totalFrames(); got != 12 {
		t.Fatalf("frames = %d, want 12 (4 connections x 3 frames)", got)
	}
	for sample := range 2 {
		if _, ok := rec.received(sample, 7); !ok {
			t.Errorf("sample %d did not record seq 7", sample)
		}
		if _, ok := rec.received(sample, 8); ok {
			t.Errorf("sample %d recorded a marker from another step", sample)
		}
	}
	joined, _ := pool.sampleState(2)
	if !joined[0] || !joined[1] {
		t.Errorf("sampled joined = %v", joined)
	}

	// server 端斷線 → 算 join 之後的斷線
	f.mu.Lock()
	f.conns[3].Close()
	f.mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for pool.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pool.closes.Load() != 1 {
		t.Errorf("closes after join = %d, want 1", pool.closes.Load())
	}
}

// 收尾時自己關的連線不算斷線
func TestCloseAllIsNotCountedAsDisconnect(t *testing.T) {
	f := newFakeWSServer(t, 10)
	pool := newViewerPool(viewerConfig{
		Servers: []string{f.srv.URL}, Rooms: []string{"r"}, Run: "x", Sampled: 1,
		RampRate: 1000, JoinTimeout: 2 * time.Second, MaxDialing: 4,
	})
	pool.rampTo(context.Background(), 3)
	pool.closeAll()
	if pool.closes.Load() != 0 {
		t.Errorf("closes = %d after closeAll, want 0", pool.closes.Load())
	}
}

// 2 個房間、2 台 server（有公因數）：每個房間的觀眾都要分散在兩台 server 上，
// 否則房間和 server 綁死，POST 到 A、廣播經 Redis 到 B 的跨 instance 路徑就沒被測到
func TestViewersOfARoomSpanServers(t *testing.T) {
	a, b := newFakeWSServer(t, 100), newFakeWSServer(t, 100)
	rooms := []string{"roomAAAAAAA", "roomBBBBBBB"}
	pool := newViewerPool(viewerConfig{
		Servers: []string{a.srv.URL, b.srv.URL}, Rooms: rooms, Run: "x", Sampled: 1,
		RampRate: 1000, JoinTimeout: 2 * time.Second, MaxDialing: 8,
	})
	defer pool.closeAll()
	if res := pool.rampTo(context.Background(), 8); res.Joined != 8 {
		t.Fatalf("ramp = %+v, want 8 joined", res)
	}

	for name, f := range map[string]*fakeWSServer{"server A": a, "server B": b} {
		perRoom := map[string]int{}
		f.mu.Lock()
		for _, j := range f.joins {
			for _, r := range rooms {
				if strings.Contains(j, r) {
					perRoom[r]++
				}
			}
		}
		f.mu.Unlock()
		for _, r := range rooms {
			if perRoom[r] != 2 {
				t.Errorf("%s has %d viewers of %s, want 2 (8 viewers: each room x each server gets 2)", name, perRoom[r], r)
			}
		}
	}
}

func TestServerIndex(t *testing.T) {
	// 1 個房間：跟原本的 i % servers 一樣
	for i := range 6 {
		if got := serverIndex(i, 1, 3); got != i%3 {
			t.Errorf("serverIndex(%d,1,3) = %d, want %d", i, got, i%3)
		}
	}
	// 2 個房間 2 台 server：房間 0 的項目 (0, 2, 4, 6) 要輪流落在 server 0, 1, 0, 1
	var got []int
	for i := 0; i < 8; i += 2 {
		got = append(got, serverIndex(i, 2, 2))
	}
	if got[0] != 0 || got[1] != 1 || got[2] != 0 || got[3] != 1 {
		t.Errorf("room 0 servers = %v, want 0 1 0 1", got)
	}
}
