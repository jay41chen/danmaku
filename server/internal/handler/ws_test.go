package handler

import (
	"testing"

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
	handler := NewWebsocketHandler(nil, nil)
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
