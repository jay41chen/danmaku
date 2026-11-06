package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 記錄每台 server 收到哪些房間的 POST
type recordingServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	rooms map[string]int
}

func newRecordingServer(t *testing.T) *recordingServer {
	r := &recordingServer{rooms: map[string]int{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.rooms[req.URL.Path]++
		r.mu.Unlock()
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// POST 輪流送到每台 server，而且每個房間都會落在兩台上（房間數和 server 數都是 2 的情況）
func TestPosterRoundRobinAcrossServersAndRooms(t *testing.T) {
	a, b := newRecordingServer(t), newRecordingServer(t)
	p := &poster{
		client:  newHTTPClient(8, 2*time.Second),
		servers: []string{a.srv.URL, b.srv.URL},
		rooms:   []string{"roomA", "roomB"},
		tokens:  []string{"t1", "t2", "t3"},
		run:     "run1",
	}
	for seq := range 8 {
		status, err := p.send(context.Background(), seq)
		if err != nil || status != http.StatusCreated {
			t.Fatalf("send %d = %d, %v", seq, status, err)
		}
	}
	for name, r := range map[string]*recordingServer{"server A": a, "server B": b} {
		r.mu.Lock()
		gotA, gotB := r.rooms["/rooms/roomA/messages"], r.rooms["/rooms/roomB/messages"]
		r.mu.Unlock()
		if gotA != 2 || gotB != 2 {
			t.Errorf("%s got roomA/roomB = %d/%d POSTs, want 2/2 (the room's POSTs must reach both servers)", name, gotA, gotB)
		}
	}
}

// sampled 連線的 expected 只算 server 回 201 的：429 / 5xx / 傳輸錯誤 / dropped 都沒有廣播；房間不同的也不算
func TestExpectedReceiveOnlyCounts201InTheSamplesRoom(t *testing.T) {
	loop := &openLoopResult{Outcome: make([]int8, 6), Status: make([]int32, 6)}
	set := func(seq int, outcome int8, status int32) { loop.Outcome[seq], loop.Status[seq] = outcome, status }
	// 2 個房間：偶數 seq = 房間 0，奇數 = 房間 1。sample 0 在房間 0
	set(0, outcomeResponse, 201)
	set(1, outcomeResponse, 201) // 房間 1：不算
	set(2, outcomeResponse, 429)
	set(3, outcomeResponse, 201)
	set(4, outcomeError, 0)
	set(5, outcomeDropped, 0)
	exp := expectedReceive(loop, 2, []int{0})
	for seq, want := range []bool{true, false, false, false, false, false} {
		if got := exp(0, seq); got != want {
			t.Errorf("expected(sample 0, seq %d) = %v, want %v", seq, got, want)
		}
	}
	set(4, outcomeResponse, 500)
	if exp(0, 4) {
		t.Error("a 500 response counts as expected")
	}
	set(4, outcomeResponse, 201)
	if !exp(0, 4) {
		t.Error("a 201 in the sample's room does not count as expected")
	}
}
