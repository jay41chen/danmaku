package handler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jay41chen/danmaku/server/internal/ws"
)

// 一條連線的業務層：分派 client 送來的 frame、限流、join / leave / auth
// 只有讀取迴圈那個 goroutine 用（ReadLoop 的 onMessage），所以欄位不需要鎖
type wsSession struct {
	h       *WebsocketHandler // 共用的服務：db / redis / tokens / hub / logger、編碼 helper
	client  *ws.Client
	limiter *frameRateLimiter
}

func newWsSession(h *WebsocketHandler, client *ws.Client) *wsSession {
	return &wsSession{h: h, client: client, limiter: newFrameRateLimiter()}
}

// 收到一個 frame：先過限流，再分派
func (s *wsSession) onMessage(ctx context.Context, msg []byte) {
	// WS 收到的 frame（auth / ping / 其他）每條連線每秒最多 rateLimit 個，超過的直接丟掉（不驗、不查、不回）
	// 跟 POST /rooms/{public_id}/messages 的限流無關：WS 連線不用登入就能連，任何人都能送 frame 逼 server 做事
	// （每個 frame 都要拿 Hub 的鎖回應；用自己合法 token 狂送 auth 還會每次查 DB）
	if !s.limiter.allow(time.Now()) {
		s.h.logger.WarnContext(ctx, "too many msgs sent", "limit", rateLimit)
		return
	}

	s.dispatch(ctx, msg)
}

// 收到 client 一個 frame 後的分派：
//   - type = "auth" → handleAuth（連線內登入 / 續期，失敗只降回訪客，不斷線）
//   - type = "ping" → 回 pong（應用層心跳，payload 忽略；一樣算進每條連線的 5 則/秒限流）
//   - type = "join" → handleJoin（進入房間，一條連線同時只能在一個房間）
//   - type = "leave" → handleLeave（離開房間）
//   - 其他（含不是合法 JSON）→ 回 ws_write_forbidden
//
// WS 不能寫入：發訊息要走 POST /rooms/{public_id}/messages（那裡才驗得了身分、存得進 DB 並帶 user_id）
// 這裡只回一個 error envelope 給「這條連線」，不存 DB、不 Publish、不關連線
// 目前對所有人一律拒絕，直到 W4b 有角色（房主 / curator）才會有人能透過 WS 寫
//
// 每條連線的 5 則/秒限流（onMessage）同時也限制了 error frame 的數量（auth frame 也算），
// 不用另外擋「狂送訊息逼 server 狂回錯誤」
func (s *wsSession) dispatch(ctx context.Context, msg []byte) {
	var in struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg, &in); err != nil {
		s.replyError(ctx, wsCodeWriteForbidden, wsMsgWriteForbidden)
		return
	}
	switch in.Type {
	case wsTypeAuth:
		s.handleAuth(ctx, in.Payload)
	case wsTypeJoin:
		s.handleJoin(ctx, in.Payload)
	case wsTypeLeave:
		s.handleLeave(ctx)
	case wsTypePing:
		s.reply(ctx, wsEnvelope{Type: wsTypePong, Payload: nil})
	default:
		s.replyError(ctx, wsCodeWriteForbidden, wsMsgWriteForbidden)
	}
}

// 回一個 envelope 給這條連線
func (s *wsSession) reply(ctx context.Context, env wsEnvelope) {
	s.h.hub.Send(ctx, s.client, s.h.encodeEnvelope(ctx, env))
}

// 回一個 error frame 給這條連線
func (s *wsSession) replyError(ctx context.Context, code, message string) {
	s.h.hub.Send(ctx, s.client, s.h.errorFrame(ctx, code, message))
}

// 處理 join frame。DB 查詢和 JoinRoom 都在 Hub 外做（Redis / DB 可能慢，不能擋住廣播和其他連線），
// 只有「記房間 + 回 frame」才呼叫 Hub
// payload 壞掉 / room 空 → 一律回 ws_room_not_found（跟 auth 壞 payload 回 ws_auth_failed 同一個做法）
func (s *wsSession) handleJoin(ctx context.Context, raw json.RawMessage) {
	if s.h.hub.Room(s.client) != "" {
		s.replyError(ctx, wsCodeAlreadyInRoom, wsMsgAlreadyInRoom)
		return
	}
	roomID, wsErr := s.h.enterRoom(ctx, raw)
	if wsErr != nil {
		s.replyError(ctx, wsErr.Code, wsErr.Message)
		return
	}
	// JoinRoom 成功：從這一刻起，斷線清理（hub.Unregister 回傳的房間）要負責 LeaveRoom
	joinOK := s.h.encodeEnvelope(ctx, wsEnvelope{Type: wsTypeJoinOK, Payload: wsRoomPayload{Room: roomID}})
	if !s.h.hub.Join(ctx, s.client, roomID, joinOK) {
		// 防禦用：現在只有讀取迴圈（就是呼叫我們的這個 goroutine）會 Unregister，照理不會發生
		// 真的發生的話，JoinRoom 已經成功了，而且沒有別人會 LeaveRoom，所以這裡要自己補
		s.h.leaveRoom(ctx, roomID)
	}
}

// 處理 leave frame。對 client 來說 leave 一定成功：先改 Hub 裡的房間並回 leave_ok，
// 再到 Hub 外做 LeaveRoom（失敗只記 log，由 roomSubscriptions 處理）
func (s *wsSession) handleLeave(ctx context.Context) {
	roomID := s.h.hub.Room(s.client)
	if roomID == "" {
		s.replyError(ctx, wsCodeNotInRoom, wsMsgNotInRoom)
		return
	}

	s.h.hub.Leave(ctx, s.client, s.h.encodeEnvelope(ctx, wsEnvelope{Type: wsTypeLeaveOK, Payload: wsRoomPayload{Room: roomID}}))

	s.h.leaveRoom(ctx, roomID)
}

// 處理 auth frame。token 解析和 DB 查詢都在 Hub 外做（DB 可能慢，不能擋住廣播和其他連線），
// 只有「套用身分變更 + 回 frame」才呼叫 Hub
// 任何失敗一律降回匿名並回 ws_auth_failed（即使原本是 member），連線不關
func (s *wsSession) handleAuth(ctx context.Context, raw json.RawMessage) {
	user, exp, ok := s.h.verifyAuthPayload(ctx, raw)
	if !ok {
		s.h.hub.Downgrade(ctx, s.client, ws.ReasonAuthFailed, s.h.errorFrame(ctx, wsCodeAuthFailed, wsMsgAuthFailed))
		return
	}
	authOK := s.h.encodeEnvelope(ctx, wsEnvelope{
		Type: wsTypeAuthOK,
		Payload: wsAuthOKPayload{
			User:      userResponse{ID: user.ID, Username: user.Username},
			ExpiresIn: int(time.Until(exp).Seconds()),
		},
	})
	expired := s.h.errorFrame(ctx, wsCodeAuthExpired, wsMsgAuthExpired)
	s.h.hub.Authenticate(ctx, s.client, user.ID, user.Username, exp, authOK, expired)
}

// 每條連線的 frame 限流：固定視窗，視窗內最多 rateLimit 個 frame
// 只有讀取迴圈那個 goroutine 會用，所以不需要鎖
type frameRateLimiter struct {
	msgCount    int
	windowStart time.Time
}

func newFrameRateLimiter() *frameRateLimiter {
	return &frameRateLimiter{windowStart: time.Now()}
}

// 回傳 false = 這個 frame 超過限流，要丟掉
func (this *frameRateLimiter) allow(now time.Time) bool {
	if now.Sub(this.windowStart) > time.Second {
		this.msgCount = 0
		this.windowStart = now
	}
	this.msgCount++
	return this.msgCount <= rateLimit
}
