package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/ws"
)

const (
	// 沒指定 WithMaxClients 時的連線上限；production 由 config 的 WS_MAX_CLIENTS（預設同為 100）傳進來
	defaultMaxClients = 100
	rateLimit         = 5

	// auth frame 查 user（CurrentUser）的上限，卡住時不能一直佔著這條連線的讀取迴圈
	wsAuthDBTimeout = 3 * time.Second

	// server 送 protocol ping 的間隔；必須小於 nginx proxy_read_timeout（60s）
	// client connection.ts 的心跳間隔要跟這個一致，範圍檢查在 ws_test.go
	wsPingPeriod = 25 * time.Second
	// read deadline：連續 3 次 ping 都沒有回應才判定連線死掉
	wsPongWait = 3 * wsPingPeriod

	// join / leave frame 的房間查詢、JoinRoom、LeaveRoom 各自的上限（每個呼叫各包一個 timeout）：
	// 卡住時不能一直佔著這條連線的讀取迴圈
	wsJoinTimeout = 200 * time.Millisecond
)

// handler 只需要「能驗 access token 並拿到 exp」：*auth.TokenIssuer 不用宣告就自動符合
// 要 exp 是因為 WS 連線是長連線，token 過期後要把身分降回訪客
type AccessTokenClaimsParser interface {
	ParseAccessTokenClaims(token string) (userID int64, expiresAt time.Time, err error)
}

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 ADR 0009）
type wsUserUseCase interface {
	CurrentUser(ctx context.Context, userID int64) (model.User, error)
}

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 ADR 0009）
type wsRoomUseCase interface {
	GetRoom(ctx context.Context, publicID string) (model.Room, error)
}

// 這個 handler 需要的 Redis 方法（為什麼定義在這裡見 ADR 0009）
type wsPubSub interface {
	Subscribe(ctx context.Context, handler func(roomID, msg string), onResync func(roomID string)) error
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
//	      ├ handleAuth → 沿用上層 ctx，但 CurrentUser 另外包 wsAuthDBTimeout（DB 卡住不能擋住讀取迴圈）
//	      ├ handleJoin（enterRoom → lookupRoom / subscribeRoom）→ 沿用上層 ctx，但房間查詢（經 room use case 的 GetRoom）、JoinRoom 各自另外包 wsJoinTimeout
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
	rooms  wsRoomUseCase
	users  wsUserUseCase
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

// WebsocketHandler 的選項：不傳就是 production 預設
type WebsocketOption func(*websocketOptions)

type websocketOptions struct {
	maxClients int
}

// 設定這台 server 的 WS 連線上限（Hub 的 cap）；n <= 0 視為沒設，沿用預設（config 已經擋掉不合法的值）
func WithMaxClients(n int) WebsocketOption {
	return func(o *websocketOptions) {
		if n > 0 {
			o.maxClients = n
		}
	}
}

// constructor
func NewWebsocketHandler(redis wsPubSub, rooms wsRoomUseCase, users wsUserUseCase, tokens AccessTokenClaimsParser, opts ...WebsocketOption) *WebsocketHandler {
	options := websocketOptions{maxClients: defaultMaxClients}
	for _, opt := range opts {
		opt(&options)
	}
	return &WebsocketHandler{
		redis:  redis,
		rooms:  rooms,
		users:  users,
		tokens: tokens,
		hub:    ws.NewHub(options.maxClients),
		logger: slog.Default().With("component", "websocket"),

		pingPeriod: wsPingPeriod,
		pongWait:   wsPongWait,
	}
}

// 一條連線的生命週期：清理只有一個 owner（listenConnection 的讀取迴圈）
//
//	各種原因 → ReadMessage 失敗 → ReadLoop return（或處理訊息時 panic）→ hub.Unregister + LeaveRoom（defer）→ HandleWebsocket return
//	                                                                          └ defer client.Close() → close(done)（PingLoop、WriteLoop 結束）+ conn.Close()（已經被 Close 過就什麼都不做）
//
// 其他人發現連線壞了只 Close() 叫醒讀取迴圈，不自己 Unregister / LeaveRoom：
//   - PingLoop：送 ping 失敗
//   - WriteLoop：寫 frame 失敗
//   - Hub.enqueueLocked（透過 client.Close）：outbox 滿了（慢 client，廣播或回覆都可能觸發）
//
// 鎖規則見 ws.Hub：Hub 底下絕不做 I/O —— JoinRoom / LeaveRoom（Redis）、DB 都在 Hub 外做完，再呼叫 Hub 更新狀態、排 frame
// 在 listenConnection 之前 return 會跳過 Unregister → Hub 裡留一筆，不要這樣寫
func (h *WebsocketHandler) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
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
		h.logger.ErrorContext(r.Context(), "ws upgrade failed", "error", err)
		return
	}
	client := ws.NewClient(conn, h.pingPeriod, h.pongWait)
	// 不管從哪裡 return 都要關：close(done) 讓 PingLoop、WriteLoop 結束，再關 conn
	// 已經被別人 Close 過（被踢、寫入失敗）也沒關係，Close 可以重複呼叫；早退（連線數滿）時 done 沒人在聽，無所謂
	defer client.Close()

	if !h.hub.Register(r.Context(), client) {
		return
	}
	go client.HandlePingLoop(r.Context())
	go client.HandleWriteLoop(r.Context())

	h.listenConnection(r.Context(), client)
}

// 讀取迴圈：跑在 HandleWebsocket 的 goroutine 上，連線活著的期間都在這裡；結束時把連線從 Hub 移除
func (h *WebsocketHandler) listenConnection(ctx context.Context, client *ws.Client) {
	session := newWsSession(h, client)
	// 清理寫成 defer、而且要在 ReadLoop 之前登記：處理訊息時 panic 也會跑（panic 會跳過後面的程式碼，只有已登記的 defer 會跑）
	// 不 recover：panic 繼續往外走，由 RecoverPanic 印 stacktrace；這裡只負責「一定要做」的清理
	// 少了它，連線會變成 Hub 裡的幽靈：一直算在房間人數裡、佔 maxClients 名額、房間沒人也不 UNSUB
	// （TestWebsocketReadLoopPanicStillCleansUp）
	defer func() {
		// ReadMessage 失敗（conn遇到錯誤）或 panic：把該連線從 Hub 移除
		// 還在房間裡就要 LeaveRoom（Hub 外做）；被踢（outbox 滿）、WriteLoop / PingLoop 寫失敗也都是 Close 之後走到這裡清理
		if room := h.hub.Unregister(ctx, client); room != "" {
			h.leaveRoom(ctx, room)
		}
	}()
	client.ReadLoop(ctx, func(msg []byte) { session.onMessage(ctx, msg) })
}

// 編碼一個 envelope 成要送出的 frame；失敗只記 log 並回傳 nil（Hub 的方法收到 nil = 只改狀態、不送 frame）
func (h *WebsocketHandler) encodeEnvelope(ctx context.Context, env wsEnvelope) []byte {
	msg, err := json.Marshal(env)
	if err != nil {
		h.logger.ErrorContext(ctx, "marshal ws envelope failed", "type", env.Type, "error", err)
		return nil
	}
	return msg
}

// error envelope 的 frame
func (h *WebsocketHandler) errorFrame(ctx context.Context, code, message string) []byte {
	return h.encodeEnvelope(ctx, wsEnvelope{
		Type:    wsTypeError,
		Payload: httpx.ErrorDetail{Code: code, Message: message},
	})
}

// 解析 payload、確認房間存在、向 Redis JoinRoom。不碰鎖。
// 成功回傳 room 的 public_id；失敗回傳要給 client 的 error（nil = 成功）
func (h *WebsocketHandler) enterRoom(ctx context.Context, raw json.RawMessage) (string, *httpx.ErrorDetail) {
	var p wsRoomPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Room == "" {
		h.logger.InfoContext(ctx, "ws join rejected", "reason", "malformed payload")
		return "", &httpx.ErrorDetail{Code: wsCodeRoomNotFound, Message: wsMsgRoomNotFound}
	}
	if wsErr := h.lookupRoom(ctx, p.Room); wsErr != nil {
		return "", wsErr
	}
	if wsErr := h.subscribeRoom(ctx, p.Room); wsErr != nil {
		return "", wsErr
	}
	return p.Room, nil
}

// 經 room use case 確認房間存在，包 wsJoinTimeout。不碰鎖。nil = 房間存在
func (h *WebsocketHandler) lookupRoom(ctx context.Context, roomID string) *httpx.ErrorDetail {
	dbCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	if _, err := h.rooms.GetRoom(dbCtx, roomID); err != nil {
		if errors.Is(err, model.ErrRoomNotFound) {
			h.logger.InfoContext(ctx, "ws join rejected", "reason", "room not found", "room", roomID)
			return &httpx.ErrorDetail{Code: wsCodeRoomNotFound, Message: wsMsgRoomNotFound}
		}
		h.logger.ErrorContext(ctx, "ws join get room failed", "error", err, "room", roomID)
		return &httpx.ErrorDetail{Code: wsCodeJoinUnavailable, Message: wsMsgJoinUnavailable}
	}
	return nil
}

// 對 Redis 做 JoinRoom，包 wsJoinTimeout。不碰鎖。nil = 成功
func (h *WebsocketHandler) subscribeRoom(ctx context.Context, roomID string) *httpx.ErrorDetail {
	joinCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	if err := h.redis.JoinRoom(joinCtx, roomID); err != nil {
		h.logger.ErrorContext(ctx, "ws join room failed", "error", err, "room", roomID)
		return &httpx.ErrorDetail{Code: wsCodeJoinUnavailable, Message: wsMsgJoinUnavailable}
	}
	return nil
}

// 對 Redis 做 LeaveRoom（絕不能在 Hub 的鎖裡呼叫），包 wsJoinTimeout
func (h *WebsocketHandler) leaveRoom(ctx context.Context, roomID string) {
	leaveCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	h.redis.LeaveRoom(leaveCtx, roomID)
}

// 解析 payload、驗 token、確認 user 還在。不碰 Hub。
// 失敗原因只記 log（不帶 token），回給 client 的一律是同一個 ws_auth_failed
func (h *WebsocketHandler) verifyAuthPayload(ctx context.Context, raw json.RawMessage) (model.User, time.Time, bool) {
	var p wsAuthPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Token == "" {
		h.logger.InfoContext(ctx, "ws auth rejected", "reason", "malformed payload")
		return model.User{}, time.Time{}, false
	}
	userID, exp, err := h.tokens.ParseAccessTokenClaims(p.Token)
	if err != nil {
		h.logger.InfoContext(ctx, "ws auth rejected", "reason", "invalid token")
		return model.User{}, time.Time{}, false
	}
	dbCtx, cancel := context.WithTimeout(ctx, wsAuthDBTimeout)
	defer cancel()
	user, err := h.users.CurrentUser(dbCtx, userID)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			// user 已被刪除，但 token 還沒過期（stateless，撤不掉）
			h.logger.InfoContext(ctx, "ws auth rejected", "reason", "user not found", "user_id", userID)
		} else {
			h.logger.ErrorContext(ctx, "ws auth get user failed", "error", err, "user_id", userID)
		}
		return model.User{}, time.Time{}, false
	}
	return user, exp, true
}

func (h *WebsocketHandler) StartBroadcast() {
	// Redis subscribe 是 server 層級的長期任務，不屬於任何 request
	// 所以要用context.Background()
	h.redis.Subscribe(context.Background(), func(roomID, msg string) {
		// 同一個 room channel 上有兩種 payload：訊息（原樣轉給房內所有人）和 user_hidden
		// （轉給房內「不是這個 user」的連線；本人不能知道自己被 ban，所以由 server 過濾，不交給 client）
		// 前綴對得上卻解析失敗的 payload 直接丟掉：fall through 的話會被當訊息廣播給所有人（包括本人）
		if userID, isHidden, valid := parseUserHidden(msg); isHidden {
			if valid {
				h.hub.BroadcastExceptUser(context.Background(), roomID, userID, []byte(msg))
			} else {
				h.logger.Warn("malformed user_hidden payload dropped", "room", roomID)
			}
			return
		}
		h.hub.Broadcast(context.Background(), roomID, []byte(msg))
	}, func(roomID string) {
		// PubSub 重建後這間房重新訂閱好了，或 go-redis 重連後房間又收到訂閱確認（reconfirm）：中間發的訊息不會再推來，叫房內的 client 自己重新撈歷史
		frame := encodeResync(roomID)
		if frame == nil {
			// 不能把 nil 交給 Broadcast（會送出空 frame）；實務上這個固定結構不會失敗
			// 沒送出就不計數：這個 metric 數的是「送出的 resync frame」
			return
		}
		prom.WSResync()
		h.hub.Broadcast(context.Background(), roomID, frame)
	})
}

func (h *WebsocketHandler) StartPresence(store wsPresence) {
	// Presence也是Server level的任務，所以一樣用context.Background()
	renew := time.NewTicker(10 * time.Second)
	defer renew.Stop()
	count := time.NewTicker(5 * time.Second)
	defer count.Stop()
	worker := newPresenceWorker(h.hub, store, h.logger)
	worker.run(context.Background(), h.hub.PresenceChanged(), renew.C, count.C)
}
