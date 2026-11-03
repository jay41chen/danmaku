package ws

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

// 不需要網路的 Client：Hub 只會對它 Enqueue（讀 send）；這些 test 的路徑不會踢人，所以不會呼叫 Close（conn 是 nil）
func newTestClient() *Client {
	return &Client{send: make(chan []byte, SendBuffer), done: make(chan struct{})}
}

// 重複 Unregister 不會重複扣總連線數，第二次回傳 ""
func TestUnregisterTwice(t *testing.T) {
	h := NewHub(10)
	c := newTestClient()
	ctx := context.Background()
	before := gaugeValue(t, prom.WebsocketConnections)

	if !h.Register(ctx, c) {
		t.Fatal("Register = false, want true")
	}
	if room := h.Unregister(ctx, c); room != "" {
		t.Errorf("first Unregister room = %q, want empty (never joined)", room)
	}
	if room := h.Unregister(ctx, c); room != "" {
		t.Errorf("second Unregister room = %q, want empty", room)
	}

	after := gaugeValue(t, prom.WebsocketConnections)
	if before != after {
		t.Errorf("before gauge %v, after gauge %v", before, after)
	}
}

// 登入成為會員（7）、exp = 現在 + ttl，回傳該 client 的 state；排在 send 的 auth_ok 先讀掉
func registerMember(t *testing.T, h *Hub, ttl time.Duration) (*Client, *clientState) {
	t.Helper()
	ctx := context.Background()
	c := newTestClient()
	if !h.Register(ctx, c) {
		t.Fatal("Register = false")
	}
	t.Cleanup(func() { h.Unregister(ctx, c) })
	h.Authenticate(ctx, c, 7, "jay", time.Now().Add(ttl), []byte("auth_ok"), []byte("expired"))
	select {
	case got := <-c.send:
		if string(got) != "auth_ok" {
			t.Fatalf("first frame = %q, want auth_ok", got)
		}
	default:
		t.Fatal("auth_ok was not queued")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return c, h.clients[c]
}

func expectNoFrame(t *testing.T, c *Client, wait time.Duration) {
	t.Helper()
	select {
	case got := <-c.send:
		t.Fatalf("unexpected frame %q", got)
	case <-time.After(wait):
	}
}

// 競態：expiry timer 已觸發、callback 卡在等 h.mu 時，連線因壞掉的 auth frame 被降級成匿名
// callback 拿到鎖後必須靠第三個 guard（st.userID == 0）直接 return，不能再送 expired
func TestExpiryTimerAfterDowngradeToAnonymous(t *testing.T) {
	h := NewHub(10)
	c, st := registerMember(t, h, 100*time.Millisecond)

	// 在 exp 之前搶下 Hub 的鎖，讓 timer callback 觸發後卡在 Lock()
	h.mu.Lock()
	time.Sleep(250 * time.Millisecond) // 超過 exp，並確保 callback 已經在等鎖

	if st.userID != 7 {
		h.mu.Unlock()
		t.Fatalf("userID = %d, want 7", st.userID)
	}
	// 持鎖期間模擬壞 auth frame 造成的降級（不能走 Downgrade：它也要拿鎖）
	h.downgradeLocked(context.Background(), c, st, ReasonAuthFailed)
	h.mu.Unlock()

	// callback 拿到鎖後不該再送 expired
	expectNoFrame(t, c, 300*time.Millisecond)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.members) != 0 {
		t.Errorf("members = %v, want empty", h.members)
	}
	if st.userID != 0 {
		t.Errorf("userID = %d, want 0 (anonymous)", st.userID)
	}
	if got := gaugeValue(t, memberGaugeForTest()); got != 0 {
		t.Errorf("member gauge = %v, want 0", got)
	}
}

// 競態：expiry timer 已觸發、callback 卡在等 h.mu 時，續期剛好成功（st.exp 被推到未來）
// callback 拿到鎖後必須靠第二個 guard（time.Now().Before(st.exp)）重查 exp 後直接 return，不能把剛續期的會員降級
func TestExpiryTimerAfterRenewal(t *testing.T) {
	h := NewHub(10)
	c, st := registerMember(t, h, 100*time.Millisecond)

	h.mu.Lock()
	time.Sleep(250 * time.Millisecond) // 超過 exp，並確保 callback 已經在等鎖
	if st.userID != 7 {
		h.mu.Unlock()
		t.Fatalf("userID = %d, want 7", st.userID)
	}
	// 持鎖期間模擬續期成功：exp 推到未來（舊 timer 的 Stop 已來不及）
	st.exp = time.Now().Add(10 * time.Second)
	h.mu.Unlock()

	// callback 拿到鎖後不該降級，也就不會送 expired
	expectNoFrame(t, c, 400*time.Millisecond)

	// 仍是會員
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.members[7] != 1 || st.userID != 7 {
		t.Errorf("members = %v, userID = %d, want member 7 kept", h.members, st.userID)
	}
}

func memberGaugeForTest() prometheus.Gauge {
	return prom.WebsocketViewers.WithLabelValues("member")
}

// 正常到期：timer 觸發 → 降回匿名並送 expired
func TestExpiryTimerDowngradesAndNotifies(t *testing.T) {
	h := NewHub(10)
	c, _ := registerMember(t, h, 50*time.Millisecond)

	select {
	case got := <-c.send:
		if string(got) != "expired" {
			t.Errorf("frame = %q, want expired", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no expired frame")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.members) != 0 {
		t.Errorf("members = %v, want empty", h.members)
	}
}
