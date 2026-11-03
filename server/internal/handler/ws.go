package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
	"github.com/jay41chen/danmaku/server/internal/ws"
)

const (
	maxClients = 100
	rateLimit  = 5

	// auth frame 查 DB（GetUserByID）的上限，卡住時不能一直佔著這條連線的讀取迴圈
	wsAuthDBTimeout = 3 * time.Second

	// server 送 protocol ping 的間隔；必須小於 nginx proxy_read_timeout（60s）
	// client connection.ts 的心跳間隔要跟這個一致，範圍檢查在 ws_test.go
	wsPingPeriod = 25 * time.Second
	// read deadline：連續 3 次 ping 都沒有回應才判定連線死掉
	wsPongWait = 3 * wsPingPeriod

	// join / leave frame 的 DB 查詢、JoinRoom、LeaveRoom 各自的上限（每個呼叫各包一個 timeout）：
	// 卡住時不能一直佔著這條連線的讀取迴圈
	wsJoinTimeout = 200 * time.Millisecond
)

// handler 只需要「能驗 access token 並拿到 exp」：*auth.TokenIssuer 不用宣告就自動符合
// 要 exp 是因為 WS 連線是長連線，token 過期後要把身分降回訪客
type AccessTokenClaimsParser interface {
	ParseAccessTokenClaims(token string) (userID int64, expiresAt time.Time, err error)
}

// 這個檔案裡的 3 種 context：
//
//	StartBroadcast        → context.Background()  process 生命週期，跟任何 request 無關
//	HandleWebsocket       → r.Context()           這條 WS 連線的生命週期，帶 request_id
//	  ├ PingLoop / WriteLoop（ws.Client）→ 沿用上層 ctx（只拿來記 log）
//	  └ listenConnection（ReadLoop）/ hub.Unregister → 沿用上層 ctx
//	    └ processRemoteMessage → 沿用上層 ctx
//	      ├ handleAuth → 沿用上層 ctx，但 GetUserByID 另外包 wsAuthDBTimeout（DB 卡住不能擋住讀取迴圈）
//	      ├ handleJoin（enterRoom → lookupRoom / subscribeRoom）→ 沿用上層 ctx，但 GetRoomByPublicID、JoinRoom 各自另外包 wsJoinTimeout
//	      ├ handleLeave → 沿用上層 ctx，但 LeaveRoom 另外包 wsJoinTimeout
//	      └ 其餘（回 error frame）沒有 DB / Redis 呼叫，不需要 timeout
//	    └ 斷線清理的 LeaveRoom → 沿用連線 ctx，另外包 wsJoinTimeout
//
// ctx 負責兩件事：1. 帶資料（request_id）2. 控制生命週期（取消、deadline）
// Background() 兩者都沒有：log 不會有 request_id，DB/Redis 卡住時也會永遠等下去
//
// 連線的 registry（clients / members / 房間 / 身分 / 到期 timer / 廣播）全部在 ws.Hub，這裡不持有任何鎖
type WebsocketHandler struct {
	redis  store.RedisStoreInterface
	db     store.DBStoreInterface
	tokens AccessTokenClaimsParser
	hub    *ws.Hub
	logger *slog.Logger
	// 預設是 wsPingPeriod / wsPongWait；欄位化只是為了讓 test 注入短時間
	pingPeriod time.Duration
	pongWait   time.Duration
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// constructor
func NewWebsocketHandler(redis store.RedisStoreInterface, db store.DBStoreInterface, tokens AccessTokenClaimsParser) *WebsocketHandler {
	return &WebsocketHandler{
		redis:  redis,
		db:     db,
		tokens: tokens,
		hub:    ws.NewHub(maxClients),
		logger: slog.Default().With("component", "websocket"),

		pingPeriod: wsPingPeriod,
		pongWait:   wsPongWait,
	}
}

// 一條連線的生命週期：清理只有一個 owner（listenConnection 的讀取迴圈）
//
//	各種原因 → ReadMessage 失敗 → ReadLoop return → hub.Unregister + LeaveRoom → HandleWebsocket return
//	                                                                          └ defer client.Shutdown() → close(done)（PingLoop、WriteLoop 結束）+ conn.Close()
//
// 其他人發現連線壞了只 Close() 叫醒讀取迴圈，不自己 Unregister / LeaveRoom：
//   - PingLoop：送 ping 失敗
//   - WriteLoop：寫 frame 失敗
//   - Hub.enqueueLocked（透過 client.Close）：outbox 滿了（慢 client，廣播或回覆都可能觸發）
//
// 鎖規則見 ws.Hub：Hub 底下絕不做 I/O —— JoinRoom / LeaveRoom（Redis）、DB 都在 Hub 外做完，再呼叫 Hub 更新狀態、排 frame
// 在 listenConnection 之前 return 會跳過 Unregister → Hub 裡留一筆，不要這樣寫
func (this *WebsocketHandler) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	// WebSocket 連線的起點其實是一個普通的 HTTP request。
	// 瀏覽器先發一個 HTTP request 說「我想升級成 WebSocket」，
	// server 同意後才切換協定。
	//
	// r.Context() 的生命週期：Hijack 之後 net/http 不再監看這條連線，
	// 所以 client 斷線時 ctx 不會被取消，要等這個 function return 才會取消
	//
	// 第三個參數一定要傳 w.Header()（不是 nil，也不是 r.Header）：
	//
	//	一般 handler  middleware: w.Header().Set(...) → handler: w.Write(...)
	//	              → net/http 自動把 w.Header() 一起送出 ✅
	//	WebSocket     middleware: w.Header().Set(...) → gorilla: Hijack() 後自己組 101 直接寫進 TCP
	//	              → 繞過 net/http，只送出這裡傳入的 header，w.Header() 不會自動送出 ❌
	//
	// 所以 middleware 設定的 header（X-Request-ID、CORS）要明確傳進去。
	// 傳 r.Header 是錯的：那是 client 送來的 request header，而且瀏覽器帶的
	// Sec-WebSocket-Extensions 會讓 gorilla 直接回 500
	conn, err := upgrader.Upgrade(w, r, w.Header())
	if err != nil {
		this.logger.ErrorContext(r.Context(), "ws upgrade failed", "error", err)
		return
	}
	client := ws.NewClient(conn, this.pingPeriod, this.pongWait)
	// 取代原本的 defer conn.Close() 和 defer close(done)：close(done) 再 conn.Close()
	// 早退（連線數滿）時 done 沒人在聽，無所謂
	defer client.Shutdown()

	if !this.hub.Register(r.Context(), client) {
		return
	}
	// 兩個goroutine共用 client 內的 `done`，Shutdown 可以一次叫醒兩個goroutine
	// 類似在kotlin裡面呼叫 `job.cancel()`的感覺
	go client.PingLoop(r.Context())
	go client.WriteLoop(r.Context())

	this.listenConnection(r.Context(), client)
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

func (this *WebsocketHandler) listenConnection(ctx context.Context, client *ws.Client) {
	limiter := newFrameRateLimiter()

	client.ReadLoop(ctx, func(msg []byte) {
		// WS 收到的 frame（auth / ping / 其他）每條連線每秒最多 rateLimit 個，超過的直接丟掉（不驗、不查、不回）
		// 跟 POST /rooms/{public_id}/messages 的限流無關：WS 連線不用登入就能連，任何人都能送 frame 逼 server 做事
		// （每個 frame 都要拿 Hub 的鎖回應；用自己合法 token 狂送 auth 還會每次查 DB）
		if !limiter.allow(time.Now()) {
			this.logger.WarnContext(ctx, "too many msgs sent", "limit", rateLimit)
			return
		}

		this.processRemoteMessage(ctx, client, msg)
	})
	// ReadMessage 失敗（conn遇到錯誤）：把該連線從 Hub 移除
	// 還在房間裡就要 LeaveRoom（Hub 外做）；被踢（outbox 滿）、WriteLoop / PingLoop 寫失敗也都是 Close 之後走到這裡清理
	if room := this.hub.Unregister(ctx, client); room != "" {
		this.leaveRoom(ctx, room)
	}
}

// 編碼一個 envelope 成要送出的 frame；失敗只記 log 並回傳 nil（Hub 的方法收到 nil = 只改狀態、不送 frame）
func (this *WebsocketHandler) encodeEnvelope(ctx context.Context, env wsEnvelope) []byte {
	msg, err := json.Marshal(env)
	if err != nil {
		this.logger.ErrorContext(ctx, "marshal ws envelope failed", "type", env.Type, "error", err)
		return nil
	}
	return msg
}

// error envelope 的 frame
func (this *WebsocketHandler) errorFrame(ctx context.Context, code, message string) []byte {
	return this.encodeEnvelope(ctx, wsEnvelope{
		Type:    wsTypeError,
		Payload: wsError{Code: code, Message: message},
	})
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
// 每條連線的 5 則/秒限流（listenConnection）同時也限制了 error frame 的數量（auth frame 也算），
// 不用另外擋「狂送訊息逼 server 狂回錯誤」
func (this *WebsocketHandler) processRemoteMessage(ctx context.Context, c *ws.Client, msg []byte) {
	var in struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg, &in); err != nil {
		this.replyError(ctx, c, wsCodeWriteForbidden, wsMsgWriteForbidden)
		return
	}
	switch in.Type {
	case wsTypeAuth:
		this.handleAuth(ctx, c, in.Payload)
	case wsTypeJoin:
		this.handleJoin(ctx, c, in.Payload)
	case wsTypeLeave:
		this.handleLeave(ctx, c)
	case wsTypePing:
		this.reply(ctx, c, wsEnvelope{Type: wsTypePong, Payload: nil})
	default:
		this.replyError(ctx, c, wsCodeWriteForbidden, wsMsgWriteForbidden)
	}
}

// 回一個 envelope 給這條連線
func (this *WebsocketHandler) reply(ctx context.Context, c *ws.Client, env wsEnvelope) {
	this.hub.Send(ctx, c, this.encodeEnvelope(ctx, env))
}

// 回一個 error frame 給這條連線
func (this *WebsocketHandler) replyError(ctx context.Context, c *ws.Client, code, message string) {
	this.hub.Send(ctx, c, this.errorFrame(ctx, code, message))
}

// 處理 join frame。DB 查詢和 JoinRoom 都在 Hub 外做（Redis / DB 可能慢，不能擋住廣播和其他連線），
// 只有「記房間 + 回 frame」才呼叫 Hub
// payload 壞掉 / room 空 → 一律回 ws_room_not_found（跟 auth 壞 payload 回 ws_auth_failed 同一個做法）
func (this *WebsocketHandler) handleJoin(ctx context.Context, c *ws.Client, raw json.RawMessage) {
	if this.hub.Room(c) != "" {
		this.replyError(ctx, c, wsCodeAlreadyInRoom, wsMsgAlreadyInRoom)
		return
	}
	roomID, wsErr := this.enterRoom(ctx, raw)
	if wsErr != nil {
		this.replyError(ctx, c, wsErr.Code, wsErr.Message)
		return
	}
	// JoinRoom 成功：從這一刻起，斷線清理（hub.Unregister 回傳的房間）要負責 LeaveRoom
	joinOK := this.encodeEnvelope(ctx, wsEnvelope{Type: wsTypeJoinOK, Payload: wsRoomPayload{Room: roomID}})
	if !this.hub.Join(ctx, c, roomID, joinOK) {
		// 防禦用：現在只有讀取迴圈（就是呼叫我們的這個 goroutine）會 Unregister，照理不會發生
		// 真的發生的話，JoinRoom 已經成功了，而且沒有別人會 LeaveRoom，所以這裡要自己補
		this.leaveRoom(ctx, roomID)
	}
}

// 解析 payload、確認房間存在、向 Redis JoinRoom。不碰鎖。
// 成功回傳 room 的 public_id；失敗回傳要給 client 的 error（nil = 成功）
func (this *WebsocketHandler) enterRoom(ctx context.Context, raw json.RawMessage) (string, *wsError) {
	var p wsRoomPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Room == "" {
		this.logger.InfoContext(ctx, "ws join rejected", "reason", "malformed payload")
		return "", &wsError{Code: wsCodeRoomNotFound, Message: wsMsgRoomNotFound}
	}
	if wsErr := this.lookupRoom(ctx, p.Room); wsErr != nil {
		return "", wsErr
	}
	if wsErr := this.subscribeRoom(ctx, p.Room); wsErr != nil {
		return "", wsErr
	}
	return p.Room, nil
}

// 查 DB 確認房間存在，包 wsJoinTimeout。不碰鎖。nil = 房間存在
func (this *WebsocketHandler) lookupRoom(ctx context.Context, roomID string) *wsError {
	dbCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	if _, err := this.db.GetRoomByPublicID(dbCtx, roomID); err != nil {
		if errors.Is(err, store.ErrRoomNotFound) {
			this.logger.InfoContext(ctx, "ws join rejected", "reason", "room not found", "room", roomID)
			return &wsError{Code: wsCodeRoomNotFound, Message: wsMsgRoomNotFound}
		}
		this.logger.ErrorContext(ctx, "ws join get room failed", "error", err, "room", roomID)
		return &wsError{Code: wsCodeJoinUnavailable, Message: wsMsgJoinUnavailable}
	}
	return nil
}

// 對 Redis 做 JoinRoom，包 wsJoinTimeout。不碰鎖。nil = 成功
func (this *WebsocketHandler) subscribeRoom(ctx context.Context, roomID string) *wsError {
	joinCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	if err := this.redis.JoinRoom(joinCtx, roomID); err != nil {
		this.logger.ErrorContext(ctx, "ws join room failed", "error", err, "room", roomID)
		return &wsError{Code: wsCodeJoinUnavailable, Message: wsMsgJoinUnavailable}
	}
	return nil
}

// 處理 leave frame。對 client 來說 leave 一定成功：先改 Hub 裡的房間並回 leave_ok，
// 再到 Hub 外做 LeaveRoom（失敗只記 log，由 roomSubscriptions 處理）
func (this *WebsocketHandler) handleLeave(ctx context.Context, c *ws.Client) {
	roomID := this.hub.Room(c)
	if roomID == "" {
		this.replyError(ctx, c, wsCodeNotInRoom, wsMsgNotInRoom)
		return
	}

	this.hub.Leave(ctx, c, this.encodeEnvelope(ctx, wsEnvelope{Type: wsTypeLeaveOK, Payload: wsRoomPayload{Room: roomID}}))

	this.leaveRoom(ctx, roomID)
}

// 對 Redis 做 LeaveRoom（絕不能在 Hub 的鎖裡呼叫），包 wsJoinTimeout
func (this *WebsocketHandler) leaveRoom(ctx context.Context, roomID string) {
	leaveCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	this.redis.LeaveRoom(leaveCtx, roomID)
}

// 處理 auth frame。token 解析和 DB 查詢都在 Hub 外做（DB 可能慢，不能擋住廣播和其他連線），
// 只有「套用身分變更 + 回 frame」才呼叫 Hub
// 任何失敗一律降回匿名並回 ws_auth_failed（即使原本是 member），連線不關
func (this *WebsocketHandler) handleAuth(ctx context.Context, c *ws.Client, raw json.RawMessage) {
	user, exp, ok := this.verifyAuthPayload(ctx, raw)
	if !ok {
		this.hub.Downgrade(ctx, c, ws.ReasonAuthFailed, this.errorFrame(ctx, wsCodeAuthFailed, wsMsgAuthFailed))
		return
	}
	authOK := this.encodeEnvelope(ctx, wsEnvelope{
		Type: wsTypeAuthOK,
		Payload: wsAuthOKPayload{
			User:      userResponse{ID: user.ID, Username: user.Username},
			ExpiresIn: int(time.Until(exp).Seconds()),
		},
	})
	expired := this.errorFrame(ctx, wsCodeAuthExpired, wsMsgAuthExpired)
	this.hub.Authenticate(ctx, c, user.ID, user.Username, exp, authOK, expired)
}

// 解析 payload、驗 token、確認 user 還在。不碰 Hub。
// 失敗原因只記 log（不帶 token），回給 client 的一律是同一個 ws_auth_failed
func (this *WebsocketHandler) verifyAuthPayload(ctx context.Context, raw json.RawMessage) (model.User, time.Time, bool) {
	var p wsAuthPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Token == "" {
		this.logger.InfoContext(ctx, "ws auth rejected", "reason", "malformed payload")
		return model.User{}, time.Time{}, false
	}
	userID, exp, err := this.tokens.ParseAccessTokenClaims(p.Token)
	if err != nil {
		this.logger.InfoContext(ctx, "ws auth rejected", "reason", "invalid token")
		return model.User{}, time.Time{}, false
	}
	dbCtx, cancel := context.WithTimeout(ctx, wsAuthDBTimeout)
	defer cancel()
	user, err := this.db.GetUserByID(dbCtx, userID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			// user 已被刪除，但 token 還沒過期（stateless，撤不掉）
			this.logger.InfoContext(ctx, "ws auth rejected", "reason", "user not found", "user_id", userID)
		} else {
			this.logger.ErrorContext(ctx, "ws auth get user failed", "error", err, "user_id", userID)
		}
		return model.User{}, time.Time{}, false
	}
	return user, exp, true
}

func (this *WebsocketHandler) StartBroadcast() {
	// Redis subscribe 是 server 層級的長期任務，不屬於任何 request
	// 所以要用context.Background()
	this.redis.Subscribe(context.Background(), func(roomID, msg string) {
		this.hub.Broadcast(context.Background(), roomID, []byte(msg))
	})
}
