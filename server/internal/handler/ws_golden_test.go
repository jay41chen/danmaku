package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// WS error frame 的 golden 測試：client/src/realtime/connection.ts 用「字面字串」比對 code，
// 所以這裡的 code 和完整 frame 都寫成字面值，不引用 handler 的常數
// （用常數的話，常數被改名時 test 會跟著一起改，抓不到合約被破壞）

var wsErrorGolden = []struct {
	name  string
	code  string // 字面 code
	frame string // 字面完整 frame（逐 byte 比對）
}{
	{"write forbidden", "ws_write_forbidden", `{"type":"error","payload":{"code":"ws_write_forbidden","message":"請透過 POST /rooms/{public_id}/messages 發送"}}`},
	{"auth failed", "ws_auth_failed", `{"type":"error","payload":{"code":"ws_auth_failed","message":"登入驗證失敗，目前為訪客"}}`},
	{"auth expired", "ws_auth_expired", `{"type":"error","payload":{"code":"ws_auth_expired","message":"登入已過期，目前為訪客"}}`},
	{"room not found", "ws_room_not_found", `{"type":"error","payload":{"code":"ws_room_not_found","message":"房間不存在"}}`},
	{"join unavailable", "ws_join_unavailable", `{"type":"error","payload":{"code":"ws_join_unavailable","message":"暫時無法進入房間，請稍後再試"}}`},
	{"already in room", "ws_already_in_room", `{"type":"error","payload":{"code":"ws_already_in_room","message":"已在房間內，請先離開"}}`},
	{"not in room", "ws_not_in_room", `{"type":"error","payload":{"code":"ws_not_in_room","message":"目前不在任何房間"}}`},
}

// 常數的值要等於字面 code；errorFrame 的輸出要逐 byte 等於字面 frame
func TestWebsocketErrorFrameGolden(t *testing.T) {
	consts := map[string][2]string{
		"write forbidden":  {wsCodeWriteForbidden, wsMsgWriteForbidden},
		"auth failed":      {wsCodeAuthFailed, wsMsgAuthFailed},
		"auth expired":     {wsCodeAuthExpired, wsMsgAuthExpired},
		"room not found":   {wsCodeRoomNotFound, wsMsgRoomNotFound},
		"join unavailable": {wsCodeJoinUnavailable, wsMsgJoinUnavailable},
		"already in room":  {wsCodeAlreadyInRoom, wsMsgAlreadyInRoom},
		"not in room":      {wsCodeNotInRoom, wsMsgNotInRoom},
	}
	if len(consts) != len(wsErrorGolden) {
		t.Fatalf("consts has %d entries, golden has %d", len(consts), len(wsErrorGolden))
	}
	h := NewWebsocketHandler(&fakeRedis{}, &wsFakeDB{}, fakeClaimsParser{})
	for _, tc := range wsErrorGolden {
		t.Run(tc.name, func(t *testing.T) {
			c := consts[tc.name]
			if c[0] != tc.code {
				t.Errorf("code const = %q, want %q", c[0], tc.code)
			}
			if got := string(h.errorFrame(t.Context(), c[0], c[1])); got != tc.frame {
				t.Errorf("frame = %s, want %s", got, tc.frame)
			}
		})
	}
}

// 讀 server 實際送出的原始 bytes（不經過 JSON decode）
func readRawFrame(t *testing.T, conn *websocket.Conn, wait time.Duration) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(wait))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read raw frame: %v", err)
	}
	return string(data)
}

func goldenFrame(t *testing.T, name string) string {
	t.Helper()
	for _, g := range wsErrorGolden {
		if g.name == name {
			return g.frame
		}
	}
	t.Fatalf("no golden frame %q", name)
	return ""
}

// 端到端：各情境實際送出的 frame 逐 byte 等於字面 JSON（確認用的是對的 code，不只是常數本身沒改）
func TestWebsocketErrorFramesOnTheWire(t *testing.T) {
	rooms := map[string]bool{"abcDEF12345": true}

	t.Run("write forbidden", func(t *testing.T) {
		_, url := newAuthTestServer(t)
		conn := dialWS(t, url)
		sendFrame(t, conn, `not json`)
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "write forbidden"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("auth failed", func(t *testing.T) {
		_, url := newAuthTestServer(t)
		conn := dialWS(t, url)
		sendAuth(t, conn, "bad")
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "auth failed"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("auth expired", func(t *testing.T) {
		_, url := newAuthTestServer(t)
		conn := dialWS(t, url)
		sendAuth(t, conn, "user7@100ms")
		expectAuthOK(t, conn, 7, "jay")
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "auth expired"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("room not found", func(t *testing.T) {
		_, url := newRoomTestServer(t, &fakeRedis{}, &wsFakeDB{})
		conn := dialWS(t, url)
		sendJoin(t, conn, "zzzZZZ99999")
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "room not found"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("join unavailable", func(t *testing.T) {
		_, url := newRoomTestServer(t, &fakeRedis{}, &wsFakeDB{roomErr: http.ErrHandlerTimeout})
		conn := dialWS(t, url)
		sendJoin(t, conn, "abcDEF12345")
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "join unavailable"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("already in room", func(t *testing.T) {
		_, url := newRoomTestServer(t, &fakeRedis{}, &wsFakeDB{rooms: rooms})
		conn := dialWS(t, url)
		joinRoom(t, conn, "abcDEF12345")
		sendJoin(t, conn, "abcDEF12345")
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "already in room"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
	t.Run("not in room", func(t *testing.T) {
		_, url := newRoomTestServer(t, &fakeRedis{}, &wsFakeDB{rooms: rooms})
		conn := dialWS(t, url)
		sendFrame(t, conn, `{"type":"leave"}`)
		if got, want := readRawFrame(t, conn, 2*time.Second), goldenFrame(t, "not in room"); got != want {
			t.Errorf("frame = %s, want %s", got, want)
		}
	})
}

// user_hidden frame 的合約：client 用字面字串 "user_hidden" 和 payload.user_id；frame 和 Redis 上的前綴都寫成字面值
func TestUserHiddenFrameGolden(t *testing.T) {
	const want = `{"type":"user_hidden","payload":{"user_id":123}}`
	if wsTypeUserHidden != "user_hidden" {
		t.Errorf("wsTypeUserHidden = %q, want user_hidden", wsTypeUserHidden)
	}
	// production 的路徑：userHiddenEnvelope → publishEnvelope（json.Marshal）→ Redis
	got, err := json.Marshal(userHiddenEnvelope(123))
	if err != nil || string(got) != want {
		t.Errorf("frame = %s (err %v), want %s", got, err, want)
	}
	if !strings.HasPrefix(want, userHiddenPrefix) || userHiddenPrefix != `{"type":"user_hidden"` {
		t.Errorf("userHiddenPrefix = %q does not match frame", userHiddenPrefix)
	}
	// 一般訊息的 frame 開頭不能被當成 user_hidden
	if strings.HasPrefix(`{"type":"message","payload":{"id":1}}`, userHiddenPrefix) {
		t.Error("message frame matches user_hidden prefix")
	}
	if id, isHidden, valid := parseUserHidden(want); id != 123 || !isHidden || !valid {
		t.Errorf("parseUserHidden = (%d, %v, %v), want (123, true, true)", id, isHidden, valid)
	}
}
