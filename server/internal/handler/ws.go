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

// 這個 handler 需要的 DB 方法（為什麼定義在這裡見 authStore）
type wsStore interface {
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)
	GetUserByID(ctx context.Context, id int64) (model.User, error)
}

// 這個 handler 需要的 Redis 方法（為什麼定義在這裡見 authStore）
type wsPubSub interface {
	Subscribe(ctx context.Context, handler func(roomID, msg string)) error
	JoinRoom(ctx context.Context, roomID string) error // 委派給 roomSubscriptions.Join
	LeaveRoom(ctx context.Context, roomID string)      // 委派給 roomSubscriptions.Leave
}

// 這個檔案裡的 3 種 context：
//
//	StartBroadcast        → context.Background()  process 生命週期，跟任何 request 無關
//	HandleWebsocket       → r.Context()           這條 WS 連線的生命週期，帶 request_id
//	  ├ PingLoop / WriteLoop（ws.Client）→ 沿用上層 ctx（只拿來記 log）
//	  └ listenConnection（ReadLoop）/ hub.Unregister → 沿用上層 ctx
//	    └ session.onMessage → dispatch → 沿用上層 ctx
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
	redis  wsPubSub
	db     wsStore
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
func NewWebsocketHandler(redis wsPubSub, db wsStore, tokens AccessTokenClaimsParser) *WebsocketHandler {
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
//	                                                                          └ defer client.Close() → close(done)（PingLoop、WriteLoop 結束）+ conn.Close()（已經被 Close 過就什麼都不做）
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
	// 不管從哪裡 return 都要關：close(done) 讓 PingLoop、WriteLoop 結束，再關 conn
	// 已經被別人 Close 過（被踢、寫入失敗）也沒關係，Close 可以重複呼叫；早退（連線數滿）時 done 沒人在聽，無所謂
	defer client.Close()

	if !this.hub.Register(r.Context(), client) {
		return
	}
	// 兩個goroutine共用 client 內的 `done`，Close 可以一次叫醒兩個goroutine
	// 類似在kotlin裡面呼叫 `job.cancel()`的感覺
	go client.PingLoop(r.Context())
	go client.WriteLoop(r.Context())

	this.listenConnection(r.Context(), client)
}

func (this *WebsocketHandler) listenConnection(ctx context.Context, client *ws.Client) {
	session := newWsSession(this, client)

	client.ReadLoop(ctx, func(msg []byte) { session.onMessage(ctx, msg) })
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
		if errors.Is(err, model.ErrRoomNotFound) {
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

// 對 Redis 做 LeaveRoom（絕不能在 Hub 的鎖裡呼叫），包 wsJoinTimeout
func (this *WebsocketHandler) leaveRoom(ctx context.Context, roomID string) {
	leaveCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	this.redis.LeaveRoom(leaveCtx, roomID)
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
		if errors.Is(err, model.ErrUserNotFound) {
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
