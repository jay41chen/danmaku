package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 讀出 gauge 目前的值（/metrics 輸出背後就是這個 dto.Metric 結構）
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper() // 失敗時錯誤訊息指向呼叫的那一行，而不是這個 helper 裡面
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestRemoveClientTwice(t *testing.T) {
	handler := NewWebsocketHandler(nil)
	conn := &websocket.Conn{}
	before := gaugeValue(t, prom.WebsocketConnections)
	handler.addClientLocked(conn)
	handler.removeClientLocked(conn)
	handler.removeClientLocked(conn)

	after := gaugeValue(t, prom.WebsocketConnections)
	if before != after {
		t.Errorf("before gauge %v, after gauge %v", before, after)
	}
}

// WS 寫入一律被拒絕：回 error envelope 給這條連線，連線保持開著，不 Publish
func TestWebsocketWriteRejected(t *testing.T) {
	redis := &fakeRedis{}
	h := NewWebsocketHandler(redis)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWebsocket))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 連續送兩次：第二次還收得到回應，證明第一次之後連線沒被關掉
	for i := range 2 {
		if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var env struct {
			Type    string  `json:"type"`
			Payload wsError `json:"payload"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
		if env.Type != wsTypeError || env.Payload.Code != wsCodeWriteForbidden || env.Payload.Message != wsMsgWriteForbidden {
			t.Errorf("reply #%d = %+v, want error envelope %q", i, env, wsCodeWriteForbidden)
		}
	}

	if len(redis.published) != 0 {
		t.Errorf("published %d messages, want 0", len(redis.published))
	}
}
