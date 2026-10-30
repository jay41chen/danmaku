package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	users   map[int64]string
	err     error
	rooms   map[string]bool // 存在的房間 public id
	roomErr error
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

// 房間：rooms 裡有的 public id 回 room，沒有的回 ErrRoomNotFound；roomErr 不是 nil 就直接回這個錯
func (f *wsFakeDB) GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error) {
	if f.roomErr != nil {
		return model.Room{}, f.roomErr
	}
	if !f.rooms[publicID] {
		return model.Room{}, store.ErrRoomNotFound
	}
	return model.Room{PublicID: publicID}, nil
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

func TestWebsocketKeepaliveConstants(t *testing.T) {
	// 下限避免太頻繁，上限要留餘裕小於 nginx proxy_read_timeout 60s；client connection.ts 的 HEARTBEAT 常數要跟著改
	if wsPingPeriod < 25*time.Second || wsPingPeriod > 55*time.Second {
		t.Errorf("wsPingPeriod = %v, want within [25s, 55s]", wsPingPeriod)
	}
	if wsPongWait < 2*wsPingPeriod {
		t.Errorf("wsPongWait = %v, want >= 2 * wsPingPeriod (%v)", wsPongWait, wsPingPeriod)
	}
}

func TestWebsocketAppPing(t *testing.T) {
	_, url := newAuthTestServer(t)
	conn := dialWS(t, url)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := readFrame(t, conn, 2*time.Second)
	if f.Type != wsTypePong {
		t.Fatalf("frame type = %q (%s), want %q", f.Type, f.Payload, wsTypePong)
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

// 競態：expiry timer 已觸發、callback 卡在等 h.mutex 時，連線因壞掉的 auth frame 被降級成匿名（收到 ws_auth_failed）
// callback 拿到鎖後必須靠第三個 guard（c.userID == 0）直接 return，不能再送 ws_auth_expired
func TestWebsocketExpiryTimerAfterDowngradeToAnonymous(t *testing.T) {
	h, url := newAuthTestServer(t)
	conn := dialWS(t, url)
	sendAuth(t, conn, "user7@100ms")
	expectAuthOK(t, conn, 7, "jay")

	// 在 exp 之前搶下全站鎖，讓 timer callback 觸發後卡在 Lock()
	h.mutex.Lock()
	time.Sleep(250 * time.Millisecond) // 超過 exp，並確保 callback 已經在等鎖

	// 持鎖期間模擬壞 auth frame 造成的降級（不能走網路：handleAuth 也要拿鎖）
	var c *wsClient
	for _, cl := range h.clients {
		c = cl
	}
	if c == nil || c.userID != 7 {
		h.mutex.Unlock()
		t.Fatalf("client = %+v, want one member client with userID 7", c)
	}
	h.downgradeLocked(context.Background(), c, wsReasonAuthFailed)
	h.mutex.Unlock()

	// callback 拿到鎖後不該再送 ws_auth_expired
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		var f wsFrame
		if err := conn.ReadJSON(&f); err != nil {
			break // timeout = 沒收到任何 frame，通過
		}
		var e wsError
		if f.Type == wsTypeError && json.Unmarshal(f.Payload, &e) == nil && e.Code == wsCodeAuthExpired {
			t.Fatalf("got %s after downgrade to anonymous, want none", wsCodeAuthExpired)
		}
	}

	waitGauge(t, memberGauge(), 0)
	waitGauge(t, prom.WebsocketViewers.WithLabelValues("anonymous"), 1)
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if len(h.members) != 0 {
		t.Errorf("members = %v, want empty", h.members)
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

// ---- keepalive（ping / pong / read deadline）----

// 跟 newAuthTestServer 一樣，但可以注入 ping 間隔和 pong 等待時間
// 必須在 httptest.NewServer 之前設定：server 啟動後再改欄位，會跟 handler goroutine 讀欄位產生 data race（-race 會抓到）
func newKeepaliveTestServer(t *testing.T, pingPeriod, pongWait time.Duration) (*WebsocketHandler, string) {
	t.Helper()
	db := &wsFakeDB{users: map[int64]string{7: "jay", 8: "amy"}}
	h := NewWebsocketHandler(&fakeRedis{}, db, fakeClaimsParser{})
	h.pingPeriod = pingPeriod
	h.pongWait = pongWait
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWebsocket))
	t.Cleanup(srv.Close)
	return h, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func clientCount(h *WebsocketHandler) int {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return len(h.clients)
}

// 輪詢等 clients 數量變成預期值：連線的建立 / 移除在 server 端的 goroutine 做
func waitForClientCount(t *testing.T, h *WebsocketHandler, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := clientCount(h)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("clients = %d, want %d (waited %v)", got, want, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 背景一直讀：gorilla 的 ping / pong handler 只有在 ReadMessage 時才會被呼叫
// conn 被 t.Cleanup 關掉後 ReadMessage 會回錯誤，goroutine 就結束
func drainInBackground(conn *websocket.Conn) {
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func TestWebsocketServerSendsPing(t *testing.T) {
	_, url := newKeepaliveTestServer(t, 50*time.Millisecond, time.Second)
	conn := dialWS(t, url)

	var pings atomic.Int32
	// 自訂 ping handler 會取代 gorilla 預設的自動回 pong，所以要自己回
	conn.SetPingHandler(func(appData string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	drainInBackground(conn)

	deadline := time.Now().Add(500 * time.Millisecond)
	for pings.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("pings = %d, want >= 2 within 500ms", pings.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebsocketDeadClientRemoved(t *testing.T) {
	// ping 設 1h：不會有 protocol ping，只有初始 read deadline 能踢掉它
	h, url := newKeepaliveTestServer(t, time.Hour, 150*time.Millisecond)
	dialWS(t, url) // 連上之後什麼都不做（不讀、不寫）

	waitForClientCount(t, h, 1, time.Second)
	waitForClientCount(t, h, 0, time.Second)
}

func TestWebsocketPongExtendsDeadline(t *testing.T) {
	h, url := newKeepaliveTestServer(t, 50*time.Millisecond, 150*time.Millisecond)
	conn := dialWS(t, url)
	drainInBackground(conn) // 預設 ping handler 會自動回 pong

	waitForClientCount(t, h, 1, time.Second)
	time.Sleep(500 * time.Millisecond) // > 3 倍 pongWait：沒有 pong 延長 deadline 早就被踢了
	if got := clientCount(h); got != 1 {
		t.Fatalf("clients = %d, want 1 (pong should extend the read deadline)", got)
	}
}

func TestWebsocketMessageExtendsDeadline(t *testing.T) {
	// ping 設 1h → 沒有 protocol pong，能活著只可能是「收到訊息就延長 deadline」
	h, url := newKeepaliveTestServer(t, time.Hour, 150*time.Millisecond)
	conn := dialWS(t, url)
	drainInBackground(conn) // 讀掉 server 回的 pong envelope

	waitForClientCount(t, h, 1, time.Second)
	// 50ms 一則 = 20 則/秒，超過每條連線 5 則/秒的限流；
	// 但 listenConnection 是在限流判斷之前就延長 deadline，被丟掉的 frame 一樣算「活著」
	for range 10 {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := clientCount(h); got != 1 {
		t.Fatalf("clients = %d, want 1 (incoming messages should extend the read deadline)", got)
	}
}

// ---- WS join / leave ----

func newRoomTestServer(t *testing.T, redis *fakeRedis, db *wsFakeDB) (*WebsocketHandler, string) {
	t.Helper()
	h := NewWebsocketHandler(redis, db, fakeClaimsParser{})
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWebsocket))
	t.Cleanup(srv.Close)
	return h, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func sendFrame(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func sendJoin(t *testing.T, conn *websocket.Conn, room string) {
	t.Helper()
	sendFrame(t, conn, fmt.Sprintf(`{"type":"join","payload":{"room":%q}}`, room))
}

func expectRoomFrame(t *testing.T, conn *websocket.Conn, wantType, wantRoom string) {
	t.Helper()
	f := readFrame(t, conn, 2*time.Second)
	var p wsRoomPayload
	if f.Type != wantType || json.Unmarshal(f.Payload, &p) != nil || p.Room != wantRoom {
		t.Fatalf("frame = %s %s, want %s room %q", f.Type, f.Payload, wantType, wantRoom)
	}
}

func joinRoom(t *testing.T, conn *websocket.Conn, room string) {
	t.Helper()
	sendJoin(t, conn, room)
	expectRoomFrame(t, conn, wsTypeJoinOK, room)
}

func TestWebsocketJoinSuccess(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)

	joinRoom(t, conn, "abc")

	if got := redis.joinedRooms(); len(got) != 1 || got[0] != "abc" {
		t.Errorf("JoinRoom calls = %v, want [abc]", got)
	}
}

func TestWebsocketJoinTwice(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true, "def": true}})
	conn := dialWS(t, url)
	joinRoom(t, conn, "abc")

	sendJoin(t, conn, "def")

	expectErrorCode(t, conn, 2*time.Second, wsCodeAlreadyInRoom)
	if got := redis.joinedRooms(); len(got) != 1 {
		t.Errorf("JoinRoom calls = %v, want only the first", got)
	}
}

func TestWebsocketJoinUnknownRoom(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{})
	conn := dialWS(t, url)

	sendJoin(t, conn, "nope")

	expectErrorCode(t, conn, 2*time.Second, wsCodeRoomNotFound)
	if got := redis.joinedRooms(); len(got) != 0 {
		t.Errorf("JoinRoom calls = %v, want none", got)
	}
}

func TestWebsocketJoinDBError(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{roomErr: errors.New("db down")})
	conn := dialWS(t, url)

	sendJoin(t, conn, "abc")

	expectErrorCode(t, conn, 2*time.Second, wsCodeJoinUnavailable)
	if got := redis.joinedRooms(); len(got) != 0 {
		t.Errorf("JoinRoom calls = %v, want none", got)
	}
}

func TestWebsocketJoinRedisError(t *testing.T) {
	redis := &fakeRedis{joinErr: errors.New("redis down")}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)

	sendJoin(t, conn, "abc")

	expectErrorCode(t, conn, 2*time.Second, wsCodeJoinUnavailable)
	// join 失敗後還不在房間裡：leave 應該回 ws_not_in_room
	sendFrame(t, conn, `{"type":"leave"}`)
	expectErrorCode(t, conn, 2*time.Second, wsCodeNotInRoom)
}

func TestWebsocketJoinMalformedPayload(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)

	for _, frame := range []string{
		`{"type":"join","payload":"abc"}`,
		`{"type":"join","payload":{"room":""}}`,
		`{"type":"join"}`,
	} {
		sendFrame(t, conn, frame)
		expectErrorCode(t, conn, 2*time.Second, wsCodeRoomNotFound)
	}
	if got := redis.joinedRooms(); len(got) != 0 {
		t.Errorf("JoinRoom calls = %v, want none", got)
	}
}

func TestWebsocketLeaveWithoutJoin(t *testing.T) {
	_, url := newRoomTestServer(t, &fakeRedis{}, &wsFakeDB{})
	conn := dialWS(t, url)

	sendFrame(t, conn, `{"type":"leave"}`)

	expectErrorCode(t, conn, 2*time.Second, wsCodeNotInRoom)
}

func TestWebsocketJoinThenLeave(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)
	joinRoom(t, conn, "abc")

	sendFrame(t, conn, `{"type":"leave"}`)

	expectRoomFrame(t, conn, wsTypeLeaveOK, "abc")
	deadline := time.Now().Add(2 * time.Second)
	for len(redis.leftRooms()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := redis.leftRooms(); len(got) != 1 || got[0] != "abc" {
		t.Errorf("LeaveRoom calls = %v, want [abc]", got)
	}
	// 離開後可以再 join
	joinRoom(t, conn, "abc")
}

func TestWebsocketDisconnectLeavesRoom(t *testing.T) {
	redis := &fakeRedis{}
	_, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)
	joinRoom(t, conn, "abc")

	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for len(redis.leftRooms()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := redis.leftRooms(); len(got) != 1 || got[0] != "abc" {
		t.Errorf("LeaveRoom calls = %v, want [abc]", got)
	}
}

// JoinRoom 成功、但 client 在 applyJoin 拿鎖之前就被移除（例如廣播寫失敗）：
// 讀取迴圈結束時仍要 LeaveRoom，Join / Leave 才會成對
func TestWebsocketJoinThenRemovedBeforeApplyLeavesRoom(t *testing.T) {
	redis := &fakeRedis{joinEntered: make(chan struct{}, 1), joinBlock: make(chan struct{})}
	// 用 Once 包住 close：test 中途失敗時 cleanup 也會放行卡住的 JoinRoom，正常流程再 close 一次不會 panic
	var releaseOnce sync.Once
	releaseJoin := func() { releaseOnce.Do(func() { close(redis.joinBlock) }) }
	t.Cleanup(releaseJoin)
	h, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"abc": true}})
	conn := dialWS(t, url)

	sendJoin(t, conn, "abc")
	select {
	case <-redis.joinEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("JoinRoom was not entered")
	}

	// JoinRoom 還卡著：模擬廣播寫失敗，把唯一的 server 端連線 Close + 移除
	h.mutex.Lock()
	if len(h.clients) != 1 {
		h.mutex.Unlock()
		t.Fatalf("clients = %d, want 1", len(h.clients))
	}
	for serverConn := range h.clients {
		serverConn.Close()
		h.removeClientLocked(serverConn)
	}
	h.mutex.Unlock()
	releaseJoin()

	deadline := time.Now().Add(2 * time.Second)
	for len(redis.leftRooms()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := redis.leftRooms(); len(got) != 1 || got[0] != "abc" {
		t.Errorf("LeaveRoom calls = %v, want [abc]", got)
	}
}

// 廣播只送給在該房間的連線
func TestWebsocketBroadcastFilteredByRoom(t *testing.T) {
	redis := &fakeRedis{}
	h, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"a": true, "b": true}})
	// fakeRedis.Subscribe 只存 handler 就返回，所以可以直接同步呼叫
	h.StartBroadcast()
	conn1 := dialWS(t, url)
	conn2 := dialWS(t, url)
	joinRoom(t, conn1, "a")
	joinRoom(t, conn2, "b")

	redis.deliver("a", `{"type":"message","payload":"hi"}`)

	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn1.ReadMessage()
	if err != nil || string(data) != `{"type":"message","payload":"hi"}` {
		t.Fatalf("conn1 read = %q, %v, want the broadcast", data, err)
	}
	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := conn2.ReadMessage(); err == nil {
		t.Fatalf("conn2 received %q, want nothing", data)
	}
}

// ---- frameRateLimiter ----

// 固定視窗限流的邊界：視窗內最多 rateLimit 個；剛好滿 1 秒還在同一個視窗（條件是嚴格 >），超過才重置
// 時間完全由 test 注入，不 sleep
func TestFrameRateLimiterWindowBoundaries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := &frameRateLimiter{windowStart: t0}

	steps := []struct {
		name  string
		at    time.Time
		count int  // 這一步連續呼叫幾次
		want  bool // 每一次呼叫預期的結果
	}{
		{"視窗內前 rateLimit 個放行", t0, rateLimit, true},
		{"第 rateLimit+1 個被擋", t0, 1, false},
		{"剛好 t0+1s 還在同一視窗，仍被擋", t0.Add(time.Second), 1, false},
		{"t0+1s+1ns 重置視窗，放行", t0.Add(time.Second + time.Nanosecond), 1, true},
		{"新視窗內剩下的額度放行", t0.Add(time.Second + time.Nanosecond), rateLimit - 1, true},
		{"新視窗額度用完，下一個被擋", t0.Add(time.Second + time.Nanosecond), 1, false},
	}
	for _, s := range steps {
		for i := range s.count {
			if got := l.allow(s.at); got != s.want {
				t.Fatalf("%s: call #%d allow = %v, want %v", s.name, i+1, got, s.want)
			}
		}
	}
}

// ---- 廣播寫失敗 ----

// 找出 client 端 conn 對應的 server 端連線（呼叫時要持有 h.mutex）：server 端的 RemoteAddr 就是 client 端的 LocalAddr
func serverConnForLocked(h *WebsocketHandler, client *websocket.Conn) *websocket.Conn {
	for serverConn := range h.clients {
		if serverConn.RemoteAddr().String() == client.LocalAddr().String() {
			return serverConn
		}
	}
	return nil
}

// 廣播時對某條連線寫失敗：廣播要把它 Close + 從 clients 移除，其他連線照收
// 用 CloseWrite 只關 server 端的寫入方向：寫會失敗，但讀取方向還活著，
// 讀取迴圈不會發現異常，所以「移除」只可能是廣播做的，不會被讀取迴圈的清理蓋掉
func TestWebsocketBroadcastWriteFailureRemovesClient(t *testing.T) {
	redis := &fakeRedis{}
	h, url := newRoomTestServer(t, redis, &wsFakeDB{rooms: map[string]bool{"a": true}})
	h.StartBroadcast()
	conn1 := dialWS(t, url)
	conn2 := dialWS(t, url)
	joinRoom(t, conn1, "a")
	joinRoom(t, conn2, "a")

	h.mutex.Lock()
	failing := serverConnForLocked(h, conn1)
	healthy := serverConnForLocked(h, conn2)
	if failing == nil || healthy == nil || len(h.clients) != 2 {
		h.mutex.Unlock()
		t.Fatalf("setup: failing=%v healthy=%v clients=%d, want both found and 2 clients", failing != nil, healthy != nil, len(h.clients))
	}
	tcp, ok := failing.NetConn().(*net.TCPConn)
	if !ok {
		h.mutex.Unlock()
		t.Fatalf("server conn is %T, want *net.TCPConn", failing.NetConn())
	}
	err := tcp.CloseWrite()
	h.mutex.Unlock()
	if err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	const msg = `{"type":"message","payload":"hi"}`
	redis.deliver("a", msg) // 同步：廣播在 mutex 下跑完才返回

	// 立刻檢查（不輪詢）：移除一定是這次廣播做的
	h.mutex.Lock()
	_, failingStill := h.clients[failing]
	_, healthyStill := h.clients[healthy]
	n := len(h.clients)
	h.mutex.Unlock()
	if failingStill {
		t.Error("寫失敗的 client 還在 h.clients，廣播沒有移除它")
	}
	if !healthyStill || n != 1 {
		t.Errorf("healthy client still in clients = %v, clients = %d, want true and 1", healthyStill, n)
	}

	// 另一條連線照樣收到
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := conn2.ReadMessage(); err != nil || string(data) != msg {
		t.Fatalf("conn2 read = %q, %v, want the broadcast", data, err)
	}

	// 廣播有 Close 失敗的連線 → 它的讀取迴圈結束 → LeaveRoom
	// （client 端的 conn1 沒有關，所以這只能是廣播 Close 造成的）
	deadline := time.Now().Add(2 * time.Second)
	for len(redis.leftRooms()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := redis.leftRooms(); len(got) != 1 || got[0] != "a" {
		t.Errorf("LeaveRoom calls = %v, want [a]", got)
	}
}
