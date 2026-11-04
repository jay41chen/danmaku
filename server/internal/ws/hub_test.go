package ws

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/randx"
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
	return &Client{ID: randx.URLText(8), send: make(chan []byte, SendBuffer), done: make(chan struct{})}
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

// 把已排隊的 wake 訊號讀掉；回傳有沒有訊號
func drainWake(h *Hub) bool {
	select {
	case <-h.PresenceChanged():
		return true
	default:
		return false
	}
}

// 快照的規則：只收在房間裡的連線，身分跟著 auth / 降級 / 離開房間即時變化
func TestPresenceSnapshot(t *testing.T) {
	h := NewHub(10)
	ctx := context.Background()
	c := newTestClient()
	h.Register(ctx, c)
	t.Cleanup(func() { h.Unregister(ctx, c) })

	want := func(label string, entries ...model.PresenceEntry) {
		t.Helper()
		got := h.PresenceSnapshot()
		if len(got) != len(entries) {
			t.Fatalf("%s: snapshot = %v, want %v", label, got, entries)
		}
		for _, e := range entries {
			if _, ok := got[e]; !ok {
				t.Fatalf("%s: snapshot = %v, want %v", label, got, entries)
			}
		}
	}

	want("registered but not in room")
	h.Join(ctx, c, "R", nil)
	want("joined anonymous", model.PresenceEntry{Room: "R", ConnID: c.ID, UserID: 0})
	h.Authenticate(ctx, c, 7, "jay", time.Now().Add(time.Hour), nil, nil)
	want("authenticated", model.PresenceEntry{Room: "R", ConnID: c.ID, UserID: 7})
	h.Downgrade(ctx, c, ReasonAuthFailed, nil)
	want("downgraded", model.PresenceEntry{Room: "R", ConnID: c.ID, UserID: 0})
	h.Leave(ctx, c, nil)
	want("left room")
}

// 所有可能改變快照的操作都要叫醒 worker（漏一個 = 人數晚更新）
func TestPresenceWakeOnChanges(t *testing.T) {
	h := NewHub(10)
	ctx := context.Background()
	c := newTestClient()
	h.Register(ctx, c)

	steps := []struct {
		name string
		do   func()
	}{
		{"join", func() { h.Join(ctx, c, "R", nil) }},
		{"auth", func() { h.Authenticate(ctx, c, 7, "jay", time.Now().Add(time.Hour), nil, nil) }},
		{"downgrade", func() { h.Downgrade(ctx, c, ReasonAuthFailed, nil) }},
		{"leave", func() { h.Leave(ctx, c, nil) }},
		{"unregister in room", func() {
			h.Join(ctx, c, "R", nil)
			drainWake(h)
			h.Unregister(ctx, c)
		}},
	}
	for _, s := range steps {
		drainWake(h)
		s.do()
		if !drainWake(h) {
			t.Errorf("%s: no wake signal", s.name)
		}
	}
}

// token 到期的 timer 降級也要叫醒 worker
func TestPresenceWakeOnExpiry(t *testing.T) {
	h := NewHub(10)
	ctx := context.Background()
	c := newTestClient()
	h.Register(ctx, c)
	t.Cleanup(func() { h.Unregister(ctx, c) })
	h.Join(ctx, c, "R", nil)
	h.Authenticate(ctx, c, 7, "jay", time.Now().Add(200*time.Millisecond), nil, nil)
	drainWake(h) // 清掉 Authenticate 本身的 wake，剩下的只會是到期那次

	select {
	case <-h.PresenceChanged():
	case <-time.After(2 * time.Second):
		t.Fatal("no wake signal after token expiry")
	}
	snap := h.PresenceSnapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	for e := range snap {
		if e.UserID != 0 {
			t.Errorf("entry %+v, want anonymous after expiry", e)
		}
	}
}

// 沒人讀 wake channel 時，連續變動不能卡住 Hub（鎖裡不能阻塞）
func TestPresenceWakeNonBlocking(t *testing.T) {
	h := NewHub(10)
	ctx := context.Background()
	c := newTestClient()
	h.Register(ctx, c)
	t.Cleanup(func() { h.Unregister(ctx, c) })

	done := make(chan struct{})
	go func() {
		h.Join(ctx, c, "R", nil)
		h.Leave(ctx, c, nil)
		h.Join(ctx, c, "S", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Hub blocked when nobody reads the wake channel")
	}
	if !drainWake(h) {
		t.Error("want a pending wake signal")
	}
	if drainWake(h) {
		t.Error("signals should coalesce into one")
	}
}
