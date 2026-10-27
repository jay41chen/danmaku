package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"
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
	handler := NewWebsocketHandler(nil, nil, nil)
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
	h := NewWebsocketHandler(redis, nil, nil)
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

// ---- WS auth ----

// 假的 claims parser：token 格式 "user<id>" 或 "user<id>@<duration>"（例如 "user7@100ms"）
// 沒寫 duration 就是 1 小時後過期；其他字串一律當作無效 token
type fakeClaimsParser struct{}

func (fakeClaimsParser) ParseAccessTokenClaims(token string) (int64, time.Time, error) {
	rest, ok := strings.CutPrefix(token, "user")
	if !ok {
		return 0, time.Time{}, errors.New("invalid token")
	}
	ttl := time.Hour
	if idPart, durPart, found := strings.Cut(rest, "@"); found {
		d, err := time.ParseDuration(durPart)
		if err != nil {
			return 0, time.Time{}, err
		}
		rest, ttl = idPart, d
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, time.Time{}, err
	}
	return id, time.Now().Add(ttl), nil
}

// 假 DB：只有 GetUserByID，users 裡沒有的 id 回 ErrUserNotFound
type wsFakeDB struct {
	store.DBStoreInterface
	users map[int64]string
	err   error
}

func (f *wsFakeDB) GetUserByID(ctx context.Context, id int64) (model.User, error) {
	if f.err != nil {
		return model.User{}, f.err
	}
	name, ok := f.users[id]
	if !ok {
		return model.User{}, store.ErrUserNotFound
	}
	return model.User{ID: id, Username: name}, nil
}

type wsFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func newAuthTestServer(t *testing.T) (*WebsocketHandler, string) {
	t.Helper()
	db := &wsFakeDB{users: map[int64]string{7: "jay", 8: "amy"}}
	h := NewWebsocketHandler(&fakeRedis{}, db, fakeClaimsParser{})
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWebsocket))
	t.Cleanup(srv.Close)
	return h, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendAuth(t *testing.T, conn *websocket.Conn, token string) {
	t.Helper()
	frame := fmt.Sprintf(`{"type":"auth","payload":{"token":%q}}`, token)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn, wait time.Duration) wsFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(wait))
	var f wsFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

func expectAuthOK(t *testing.T, conn *websocket.Conn, wantID int64, wantName string) wsAuthOKPayload {
	t.Helper()
	f := readFrame(t, conn, 2*time.Second)
	if f.Type != wsTypeAuthOK {
		t.Fatalf("frame type = %q (%s), want %q", f.Type, f.Payload, wsTypeAuthOK)
	}
	var p wsAuthOKPayload
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatalf("decode auth_ok: %v", err)
	}
	if p.User.ID != wantID || p.User.Username != wantName {
		t.Errorf("auth_ok user = %+v, want id %d name %q", p.User, wantID, wantName)
	}
	// 短效期的 case（100ms）expires_in 取整數秒會是 0，所以這裡只擋負數；>0 由 success test 另外驗
	if p.ExpiresIn < 0 {
		t.Errorf("expires_in = %d, want >= 0", p.ExpiresIn)
	}
	return p
}

func expectErrorCode(t *testing.T, conn *websocket.Conn, wait time.Duration, wantCode string) {
	t.Helper()
	f := readFrame(t, conn, wait)
	var e wsError
	if f.Type != wsTypeError || json.Unmarshal(f.Payload, &e) != nil || e.Code != wantCode {
		t.Fatalf("frame = %s %s, want error %q", f.Type, f.Payload, wantCode)
	}
}

// 等 gauge 變成預期值：連線的建立 / 移除在 server 端的 goroutine 做，test 這邊要輪詢
func waitGauge(t *testing.T, g prometheus.Gauge, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := gaugeValue(t, g)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gauge = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func memberGauge() prometheus.Gauge {
	return prom.WebsocketViewers.WithLabelValues("member")
}

// 需要 TODO(user) 完成才會過（refcount：member gauge）
func TestWebsocketAuthSuccess(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	sendAuth(t, conn, "user7")

	p := expectAuthOK(t, conn, 7, "jay")
	if p.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d, want > 0", p.ExpiresIn)
	}
	waitGauge(t, memberGauge(), 1)
}

func TestWebsocketAuthBadToken(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	// 連續兩次：第二次還收得到回應，證明 auth 失敗沒有關連線
	for range 2 {
		sendAuth(t, conn, "bad")
		expectErrorCode(t, conn, 2*time.Second, wsCodeAuthFailed)
	}
}

func TestWebsocketAuthMalformedPayload(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	for _, frame := range []string{
		`{"type":"auth"}`,
		`{"type":"auth","payload":{}}`,
		`{"type":"auth","payload":"x"}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
		expectErrorCode(t, conn, 2*time.Second, wsCodeAuthFailed)
	}
}

func TestWebsocketAuthMemberThenBadTokenBecomesAnonymous(t *testing.T) {
	h, url := newAuthTestServer(t)
	conn := dialWS(t, url)
	sendAuth(t, conn, "user7")
	expectAuthOK(t, conn, 7, "jay")

	sendAuth(t, conn, "bad")

	expectErrorCode(t, conn, 2*time.Second, wsCodeAuthFailed)
	h.mutex.Lock()
	defer h.mutex.Unlock()
	for _, c := range h.clients {
		if c.userID != 0 {
			t.Errorf("userID = %d, want 0 (anonymous)", c.userID)
		}
	}
}

func TestWebsocketAuthSwitchUser(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)
	sendAuth(t, conn, "user7")
	expectAuthOK(t, conn, 7, "jay")

	sendAuth(t, conn, "user8")

	expectAuthOK(t, conn, 8, "amy")
}

func TestWebsocketAuthUserNotFound(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	sendAuth(t, conn, "user99") // token 有效，但 DB 裡沒有這個 user

	expectErrorCode(t, conn, 2*time.Second, wsCodeAuthFailed)
}

func TestWebsocketAuthDBError(t *testing.T) {
	h, url := newAuthTestServer(t)
	h.db.(*wsFakeDB).err = errors.New("db down")
	conn := dialWS(t, url)

	sendAuth(t, conn, "user7")

	expectErrorCode(t, conn, 2*time.Second, wsCodeAuthFailed)
}

func TestWebsocketNonAuthFramesForbidden(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	for _, frame := range []string{`not json`, `{"type":"message","payload":"hi"}`} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
		expectErrorCode(t, conn, 2*time.Second, wsCodeWriteForbidden)
	}
}

// 需要 TODO(user) 完成才會過（timer）
func TestWebsocketAuthExpired(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)
	sendAuth(t, conn, "user7@100ms")
	expectAuthOK(t, conn, 7, "jay")

	expectErrorCode(t, conn, 300*time.Millisecond, wsCodeAuthExpired)
}

// 需要 TODO(user) 完成才會過（timer：續期要重設，舊的 timer 不能再觸發）
func TestWebsocketAuthRenewalBeforeExpiry(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)
	sendAuth(t, conn, "user7@150ms")
	expectAuthOK(t, conn, 7, "jay")

	sendAuth(t, conn, "user7@10s")
	expectAuthOK(t, conn, 7, "jay")

	// 原本的 150ms 過了也不該收到 ws_auth_expired
	conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	var f wsFrame
	if err := conn.ReadJSON(&f); err == nil {
		t.Fatalf("unexpected frame after renewal: %s %s", f.Type, f.Payload)
	}
}

// 需要 TODO(user) 完成才會過（refcount）
func TestWebsocketMemberRefcount(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn1 := dialWS(t, url)
	conn2 := dialWS(t, url)
	sendAuth(t, conn1, "user7")
	expectAuthOK(t, conn1, 7, "jay")
	sendAuth(t, conn2, "user7")
	expectAuthOK(t, conn2, 7, "jay")
	waitGauge(t, memberGauge(), 1) // 同一個 user 兩條連線只算 1 個 member

	conn1.Close()
	time.Sleep(100 * time.Millisecond)
	waitGauge(t, memberGauge(), 1) // 還有一條連線

	conn2.Close()
	waitGauge(t, memberGauge(), 0)
}
