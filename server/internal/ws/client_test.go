package ws

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 一條 server 端連線結束（ReadLoop return、Unregister 完）的紀錄
type closedConn struct {
	client *Client
	room   string // hub.Unregister 回傳的房間
}

// 真的 websocket 伺服器：每條連線 Register + Join(room) + WriteLoop + ReadLoop，ReadLoop 結束後 Unregister
// 跟 handler.HandleWebsocket 的流程一樣，只是沒有 JSON protocol
type hubServer struct {
	hub    *Hub
	url    string
	opened chan *Client    // 每條連線 Register + Join 完成後送出
	closed chan closedConn // 每條連線清理完成後送出
}

func newHubServer(t *testing.T, room string) *hubServer {
	t.Helper()
	s := &hubServer{hub: NewHub(10), opened: make(chan *Client, 8), closed: make(chan closedConn, 8)}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ctx := context.Background()
		c := NewClient(conn, time.Hour, time.Hour)
		defer c.Shutdown()
		if !s.hub.Register(ctx, c) {
			return
		}
		s.hub.Join(ctx, c, room, nil)
		go c.WriteLoop(ctx)
		s.opened <- c
		c.ReadLoop(ctx, func([]byte) {})
		s.closed <- closedConn{client: c, room: s.hub.Unregister(ctx, c)}
	}))
	t.Cleanup(srv.Close)
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return s
}

// 連一條 client，回傳 client 端 conn 和對應的 server 端 Client（一條一條連，所以 opened 的順序就是對應關係）
func (s *hubServer) dial(t *testing.T) (*websocket.Conn, *Client) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	select {
	case c := <-s.opened:
		return conn, c
	case <-time.After(2 * time.Second):
		t.Fatal("server did not register the client")
		return nil, nil
	}
}

func (s *hubServer) waitClosed(t *testing.T, wait time.Duration) closedConn {
	t.Helper()
	select {
	case cc := <-s.closed:
		return cc
	case <-time.After(wait):
		t.Fatal("no connection was cleaned up")
		return closedConn{}
	}
}

// WriteLoop 對某條連線寫失敗：Close 它 → 讀取迴圈醒來清理（Unregister），其他連線照收
// 用 CloseWrite 只關 server 端的寫入方向：寫會失敗，但讀取方向還活著，
// 讀取迴圈自己不會發現異常，所以它會醒來只可能是 WriteLoop 的 Close 造成的
// 「LeaveRoom == [a]」的部分由 handler 的 TestWebsocketDisconnectLeavesRoom 涵蓋；這裡只驗 Unregister 回傳 "a"（欠的 LeaveRoom）
func TestWriteFailureClosesConnection(t *testing.T) {
	s := newHubServer(t, "a")
	conn1, failing := s.dial(t)
	_ = conn1
	conn2, healthy := s.dial(t)

	tcp, ok := failing.conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("server conn is %T, want *net.TCPConn", failing.conn.NetConn())
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	msg := []byte(`{"type":"message","payload":"hi"}`)
	s.hub.Broadcast(context.Background(), "a", msg)

	// 另一條連線照樣收到
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := conn2.ReadMessage(); err != nil || string(data) != string(msg) {
		t.Fatalf("conn2 read = %q, %v, want the broadcast", data, err)
	}

	// 廣播本身不移除 client（只排進 outbox）：移除是 WriteLoop Close → 讀取迴圈清理，要等
	cc := s.waitClosed(t, 2*time.Second)
	if cc.client != failing || cc.room != "a" {
		t.Errorf("cleaned up client is failing = %v, room = %q, want the failing one and room a", cc.client == failing, cc.room)
	}
	if got := s.hub.Room(failing); got != "" {
		t.Errorf("failing room = %q, want unregistered", got)
	}
	if got := s.hub.Room(healthy); got != "a" {
		t.Errorf("healthy room = %q, want still registered in a", got)
	}
}

// 慢 client（一直不讀）：廣播不能被它卡住；它的 outbox 滿了就被踢，同房的正常 client 照收
//
// 讓 slow 的 WriteLoop 卡住：client 端從不讀 → TCP buffer 滿 → WriteMessage 卡在 writeWait（10 秒）
// → 後面的訊息全堆在 outbox。buffer 多大看 OS，所以不算「剛好第幾則會滿」，而是一直送到它被踢為止（有上限）
//
// 每送一則都等 fast 讀到才送下一則：fast 的 outbox 最多只有 1 則，不會也被踢
// （不等的話 Broadcast 只是排進 outbox、非常快，一口氣送超過 SendBuffer 則，正常 client 也會被踢 —— 這是設計）
// 「LeaveRoom == [a]」的部分由 handler 的 TestWebsocketDisconnectLeavesRoom 涵蓋；這裡驗 Unregister 回傳 "a"
func TestSlowClientKickedWithoutBlockingBroadcast(t *testing.T) {
	s := newHubServer(t, "a")
	_, slowServer := s.dial(t)
	fast, fastServer := s.dial(t)

	// 大訊息：少送幾則就能塞滿 TCP buffer
	msg := []byte(`{"type":"message","payload":"` + strings.Repeat("x", 64<<10) + `"}`)
	// 上限：TCP buffer（OS 自動調整，loopback 通常幾 MB）+ outbox（SendBuffer 則）還綽綽有餘
	const maxDeliveries = 2000
	// 舊設計裡廣播會同步寫 slow、卡滿 writeWait（10 秒）；新設計 Broadcast 只排進 outbox，應該遠低於這個值
	const maxDeliverTime = 500 * time.Millisecond

	var cleaned *closedConn
	var slowest time.Duration
	for i := 0; i < maxDeliveries && cleaned == nil; i++ {
		start := time.Now()
		s.hub.Broadcast(context.Background(), "a", msg)
		slowest = max(slowest, time.Since(start))

		fast.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, data, err := fast.ReadMessage(); err != nil || len(data) != len(msg) {
			t.Fatalf("delivery #%d: fast read len = %d, %v, want the broadcast", i+1, len(data), err)
		}

		select {
		case cc := <-s.closed:
			cleaned = &cc
		default:
		}
	}

	if slowest > maxDeliverTime {
		t.Errorf("slowest Broadcast took %v, want < %v (broadcast must not wait on a slow client)", slowest, maxDeliverTime)
	}
	if cleaned == nil {
		// 最後一次 Broadcast 剛踢完，清理可能還在路上
		select {
		case cc := <-s.closed:
			cleaned = &cc
		case <-time.After(2 * time.Second):
			t.Fatalf("slow client still connected after %d deliveries, want kicked when its outbox (%d) is full", maxDeliveries, SendBuffer)
		}
	}
	// 被踢 = Close → 讀取迴圈清理 → Unregister 回傳欠的房間
	if cleaned.client != slowServer || cleaned.room != "a" {
		t.Errorf("cleaned up client is slow = %v, room = %q, want slow and room a", cleaned.client == slowServer, cleaned.room)
	}
	// fast 沒被波及
	if got := s.hub.Room(fastServer); got != "a" {
		t.Errorf("fast room = %q, want still registered in a", got)
	}
}
