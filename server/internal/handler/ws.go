package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// WS 上跑的訊息都包成 envelope：{"type": ..., "payload": ...}
// client 靠 type 分辨收到的是廣播的訊息還是錯誤，之後要加新種類（presence 等）也不用改格式
const (
	maxClients      = 100
	maxMessageBytes = 500
	rateLimit       = 5
	writeWait       = 10 * time.Second
	// auth frame 查 DB（GetUserByID）的上限，卡住時不能一直佔著這條連線的讀取迴圈
	wsAuthDBTimeout = 3 * time.Second
	wsTypeMessage   = "message"
	wsTypeError     = "error"
	wsTypeAuth      = "auth"     // client → server：連線內登入 / 續期
	wsTypeAuthOK    = "auth_ok"  // server → client：登入 / 續期成功
	wsTypePing      = "ping"     // client → server：應用層心跳
	wsTypePong      = "pong"     // server → client：回應心跳
	wsTypeJoin      = "join"     // client → server：進入房間
	wsTypeJoinOK    = "join_ok"  // server → client：進入房間成功
	wsTypeLeave     = "leave"    // client → server：離開房間
	wsTypeLeaveOK   = "leave_ok" // server → client：離開房間成功

	// server 送 protocol ping 的間隔；必須小於 nginx proxy_read_timeout（60s）
	// client connection.ts 的心跳間隔要跟這個一致，範圍檢查在 ws_test.go
	wsPingPeriod = 25 * time.Second
	// read deadline：連續 3 次 ping 都沒有回應才判定連線死掉
	wsPongWait = 3 * wsPingPeriod

	wsCodeWriteForbidden  = "ws_write_forbidden"
	wsMsgWriteForbidden   = "請透過 POST /messages 發送"
	wsCodeAuthFailed      = "ws_auth_failed"
	wsMsgAuthFailed       = "登入驗證失敗，目前為訪客"
	wsCodeAuthExpired     = "ws_auth_expired"
	wsMsgAuthExpired      = "登入已過期，目前為訪客"
	wsCodeRoomNotFound    = "ws_room_not_found"
	wsMsgRoomNotFound     = "房間不存在"
	wsCodeJoinUnavailable = "ws_join_unavailable"
	wsMsgJoinUnavailable  = "暫時無法進入房間，請稍後再試"
	wsCodeAlreadyInRoom   = "ws_already_in_room"
	wsMsgAlreadyInRoom    = "已在房間內，請先離開"
	wsCodeNotInRoom       = "ws_not_in_room"
	wsMsgNotInRoom        = "目前不在任何房間"

	// join / leave frame 的 DB 查詢、JoinRoom、LeaveRoom 各自的上限（每個呼叫各包一個 timeout）：
	// 卡住時不能一直佔著這條連線的讀取迴圈
	wsJoinTimeout = 200 * time.Millisecond

	// 身分變更 log 用的 from / to（有登入時用 user id）
	wsAnonymous = "anonymous"

	// 身分變更的原因（log 的 reason 欄位）
	wsReasonAuth       = "auth"
	wsReasonExpired    = "expired"
	wsReasonAuthFailed = "auth_failed"
)

type wsEnvelope struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// type = "error" 的 payload：code 給程式判斷，message 給人看
type wsError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// type = "auth" 的 payload（client → server）
type wsAuthPayload struct {
	Token string `json:"token"`
}

// type = "join"（client → server）以及 "join_ok" / "leave_ok"（server → client）的 payload：room 是房間的 public_id
type wsRoomPayload struct {
	Room string `json:"room"`
}

// type = "auth_ok" 的 payload：user 沿用 REST 的 userResponse，不用 loginResponse（WS 上沒有 token 要回）
// expires_in = 剩餘整數秒，跟 REST login 的 expires_in 同單位
type wsAuthOKPayload struct {
	User      userResponse `json:"user"`
	ExpiresIn int          `json:"expires_in"`
}

// handler 只需要「能驗 access token 並拿到 exp」：*auth.TokenIssuer 不用宣告就自動符合
// 要 exp 是因為 WS 連線是長連線，token 過期後要把身分降回訪客
type AccessTokenClaimsParser interface {
	ParseAccessTokenClaims(token string) (userID int64, expiresAt time.Time, err error)
}

// 一條連線的身分狀態。userID == 0 代表匿名（users.id 是 IDENTITY，從 1 開始，不會有 0）
// 所有欄位（含 timer）都由 WebsocketHandler.mutex 保護，不另外加每連線的鎖：
// 寫一個 frame 本來就要拿全域鎖（gorilla 同一條連線只能一個 writer，廣播也在這把鎖下寫所有 client），
// 所以每連線的身分鎖現在省不了任何東西。每連線的鎖留到 W6 Hub 重構（見 ROADMAP），
// 到時的鎖順序是 全域 → 每連線
type wsClient struct {
	conn     *websocket.Conn
	userID   int64
	username string
	exp      time.Time
	timer    *time.Timer
	// 目前所在房間的 public_id，空字串 = 不在任何房間。只用來過濾廣播
	// 「當前房間」的權威狀態在讀取迴圈的 wsConnState，這裡只是給廣播看的副本
	room string
}

// 一條連線在讀取迴圈 goroutine 裡的私有狀態：只有 listenConnection 那個 goroutine 讀寫，所以不需要鎖
// room 是這條連線已經向 Redis JoinRoom 成功、斷線時需要 LeaveRoom 的房間
type wsConnState struct {
	room string
}

// 這個檔案裡的 3 種 context：
//
//	StartBroadcast        → context.Background()  process 生命週期，跟任何 request 無關
//	HandleWebsocket       → r.Context()           這條 WS 連線的生命週期，帶 request_id
//	  └ listenConnection / closeConnection / removeClient → 沿用上層 ctx
//	    └ processRemoteMessage → 沿用上層 ctx
//	      ├ handleAuth → 沿用上層 ctx，但 GetUserByID 另外包 wsAuthDBTimeout（DB 卡住不能擋住讀取迴圈）
//	      ├ handleJoin（enterRoom → lookupRoom / subscribeRoom）→ 沿用上層 ctx，但 GetRoomByPublicID、JoinRoom 各自另外包 wsJoinTimeout
//	      ├ handleLeave → 沿用上層 ctx，但 LeaveRoom 另外包 wsJoinTimeout
//	      └ 其餘（回 error frame）沒有 DB / Redis 呼叫，不需要 timeout
//	    └ 斷線清理的 LeaveRoom → 沿用連線 ctx，另外包 wsJoinTimeout
//
// ctx 負責兩件事：1. 帶資料（request_id）2. 控制生命週期（取消、deadline）
// Background() 兩者都沒有：log 不會有 request_id，DB/Redis 卡住時也會永遠等下去
type WebsocketHandler struct {
	redis  store.RedisStoreInterface
	db     store.DBStoreInterface
	tokens AccessTokenClaimsParser
	// clients / members 以及每個 wsClient 的欄位都由 mutex 保護（理由見 wsClient）
	clients map[*websocket.Conn]*wsClient
	members map[int64]int // user id → 這個 user 在這台 server 上的連線數（同一個人多分頁只算一個 viewer）
	mutex   sync.Mutex
	logger  *slog.Logger
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
		redis:   redis,
		db:      db,
		tokens:  tokens,
		clients: make(map[*websocket.Conn]*wsClient),
		members: make(map[int64]int),
		logger:  slog.Default().With("component", "websocket"),

		pingPeriod: wsPingPeriod,
		pongWait:   wsPongWait,
	}
}

// 一條連線的生命週期：清理只有一個 owner（listenConnection 的讀取迴圈）
//
//	各種原因 → ReadMessage 失敗 → closeConnection（removeClient）→ HandleWebsocket return
//	                                                               ├ defer close(done) → pingLoop 結束
//	                                                               └ defer conn.Close()
//
// 其他 goroutine（pingLoop）發現連線壞了只 Close() 叫醒讀取迴圈，不自己 removeClient
// 例外：StartBroadcast 持有全域鎖、會立刻 removeClientLocked（重複呼叫是安全的）
// 但它不能呼叫 LeaveRoom（Redis I/O 不能在鎖內做），離開房間一律由讀取迴圈結束後的清理負責
//
// 鎖規則：絕不在持有 this.mutex 時呼叫 JoinRoom / LeaveRoom（Redis I/O）或 DB。
// 一律照 handleAuth 的做法：I/O 在鎖外做完，再拿鎖更新 wsClient、回 frame
// 在 listenConnection 之前 return 會跳過 removeClient → clients 裡留一筆，不要這樣寫
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
	defer conn.Close()

	if !this.tryAddClient(r.Context(), conn) {
		return
	}
	this.setupKeepalive(r.Context(), conn)
	// 加上ping loop
	done := make(chan struct{})
	defer close(done)
	go this.pingLoop(r.Context(), conn, done)

	this.listenConnection(r.Context(), conn)
}

// 連線成功後的設定：每次發送的上限 + pingpong 的 keepalive mechanism（初始 read deadline、pong handler）
func (this *WebsocketHandler) setupKeepalive(ctx context.Context, conn *websocket.Conn) {
	// 連線成功，設定每次發送的上限
	conn.SetReadLimit(maxMessageBytes)
	// 加上pingpong的keepalive mechanism
	if err := this.addPongReadDeadline(conn); err != nil {
		// 不 return：addPongReadDeadline 失敗時已經 Close，讓之後的 listenConnection 讀取失敗後去 removeClient
		// 在這裡 return 會跳過 removeClient，clients 裡會留一筆
		this.logger.WarnContext(ctx, "set read deadline failed", "error", err)
	}

	// 掛上pong handler
	conn.SetPongHandler(func(appData string) error {
		err := this.addPongReadDeadline(conn)
		return err
	})
}

func (this *WebsocketHandler) pingLoop(ctx context.Context, conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(this.pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			// 直接結束goroutine，
			// 因為done是在HandleWebsocket中被defer close的
			// 也就是說會進到這狀況，大家都知道connection已經死了
			// 所以不需要再次handle
			return
		case <-ticker.C:
			// 送ping
			deadline := time.Now().Add(writeWait)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				// 送ping的時候遇到錯誤，代表該connection遇到問題
				// 也可能是等 gorilla 內部寫入鎖逾時（StartBroadcast 正卡在寫這條慢 client，最多 writeWait）：一律視為連線不健康、關掉
				this.logger.WarnContext(ctx, "write ping failed", "error", err)
				// 特別注意，因為pingLoop跟listenConnection在不同go routine
				// 所以這邊要特別指定close，不然listenConnection要等到timeout
				// 才會繼續走
				conn.Close()
				return
			}
		}

	}
}

func (this *WebsocketHandler) addPongReadDeadline(conn *websocket.Conn) error {
	t := time.Now().Add(this.pongWait)
	err := conn.SetReadDeadline(t)
	if err != nil {
		conn.Close()
		return err
	}
	return nil
}

// 回傳 false = 連線數已滿，沒加進去（HandleWebsocket 要直接 return）
// 抽成獨立 function 是為了用 defer Unlock：HandleWebsocket 後面的 listenConnection 會跑到斷線，不能在那裡 defer
func (this *WebsocketHandler) tryAddClient(ctx context.Context, conn *websocket.Conn) bool {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	if len(this.clients) >= maxClients {
		this.logger.WarnContext(ctx, "too many connections")
		return false
	}
	this.addClientLocked(conn)
	// request_id 在 upgrade 時產生，同一條連線的所有 log 共用，等同 connection ID
	this.logger.InfoContext(ctx, "new client connected", "total", len(this.clients))
	return true
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

func (this *WebsocketHandler) listenConnection(ctx context.Context, conn *websocket.Conn) {
	limiter := newFrameRateLimiter()
	state := &wsConnState{}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// conn遇到錯誤, 把該conn移除
			this.closeConnection(ctx, conn, state)
			return
		}
		// 新增：收到任何訊息都代表 client 還活著
		if err := this.addPongReadDeadline(conn); err != nil {
			this.logger.WarnContext(ctx, "set read deadline failed", "error", err)
		}
		// auth frame 裡有 access token，不能進 log；誰送的看身分變化的 INFO log
		this.logger.DebugContext(ctx, "frame received", "bytes", len(msg))
		// WS 收到的 frame（auth / ping / 其他）每條連線每秒最多 rateLimit 個，超過的直接丟掉（不驗、不查、不回）
		// 跟 POST /messages 的限流無關：WS 連線不用登入就能連，任何人都能送 frame 逼 server 做事
		// （每個 frame 都要拿全域鎖回應；用自己合法 token 狂送 auth 還會每次查 DB）
		if !limiter.allow(time.Now()) {
			this.logger.WarnContext(ctx, "too many msgs sent", "limit", rateLimit)
			continue
		}

		this.processRemoteMessage(ctx, conn, state, msg)
	}
}

// 斷線清理（讀取迴圈結束時呼叫）：先 removeClient，還在房間裡再 LeaveRoom
func (this *WebsocketHandler) closeConnection(ctx context.Context, conn *websocket.Conn, state *wsConnState) {
	this.removeClient(ctx, conn)
	// 還在房間裡就要 LeaveRoom（鎖外做；廣播寫失敗移除 client 時也是靠這裡清理）
	if state.room != "" {
		this.leaveRoom(ctx, state.room)
		state.room = ""
	}
}

func (this *WebsocketHandler) removeClient(ctx context.Context, conn *websocket.Conn) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.removeClientLocked(conn)
	this.logger.InfoContext(ctx, "client disconnected", "total", len(this.clients))
}

// 收到 client 一個 frame 後的分派：
//   - type = "auth" → handleAuth（連線內登入 / 續期，失敗只降回訪客，不斷線）
//   - type = "ping" → 回 pong（應用層心跳，payload 忽略；一樣算進每條連線的 5 則/秒限流）
//   - type = "join" → handleJoin（進入房間，一條連線同時只能在一個房間）
//   - type = "leave" → handleLeave（離開房間）
//   - 其他（含不是合法 JSON）→ 回 ws_write_forbidden
//
// WS 不能寫入：發訊息要走 POST /messages（那裡才驗得了身分、存得進 DB 並帶 user_id）
// 這裡只回一個 error envelope 給「這條連線」，不存 DB、不 Publish、不關連線
// 目前對所有人一律拒絕，直到 W4b 有角色（房主 / curator）才會有人能透過 WS 寫
//
// 每條連線的 5 則/秒限流（listenConnection）同時也限制了 error frame 的數量（auth frame 也算），
// 不用另外擋「狂送訊息逼 server 狂回錯誤」
func (this *WebsocketHandler) processRemoteMessage(ctx context.Context, conn *websocket.Conn, state *wsConnState, msg []byte) {
	var in struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg, &in); err != nil {
		this.replyError(ctx, conn, wsCodeWriteForbidden, wsMsgWriteForbidden)
		return
	}
	switch in.Type {
	case wsTypeAuth:
		this.handleAuth(ctx, conn, in.Payload)
	case wsTypeJoin:
		this.handleJoin(ctx, conn, state, in.Payload)
	case wsTypeLeave:
		this.handleLeave(ctx, conn, state)
	case wsTypePing:
		this.reply(ctx, conn, wsEnvelope{Type: wsTypePong, Payload: nil})
	default:
		this.replyError(ctx, conn, wsCodeWriteForbidden, wsMsgWriteForbidden)
	}
}

// 回一個 envelope 給這條連線（內部拿鎖）
func (this *WebsocketHandler) reply(ctx context.Context, conn *websocket.Conn, env wsEnvelope) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.writeEnvelopeLocked(ctx, conn, env)
}

// 回一個 error frame 給這條連線（內部拿鎖）
func (this *WebsocketHandler) replyError(ctx context.Context, conn *websocket.Conn, code, message string) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.writeErrorLocked(ctx, conn, code, message)
}

// 處理 join frame。DB 查詢和 JoinRoom 都在鎖外做（Redis / DB 可能慢，不能擋住廣播和其他連線），
// 只有「更新 wsClient.room + 回 frame」才拿鎖
// payload 壞掉 / room 空 → 一律回 ws_room_not_found（跟 auth 壞 payload 回 ws_auth_failed 同一個做法）
func (this *WebsocketHandler) handleJoin(ctx context.Context, conn *websocket.Conn, state *wsConnState, raw json.RawMessage) {
	if state.room != "" {
		this.replyError(ctx, conn, wsCodeAlreadyInRoom, wsMsgAlreadyInRoom)
		return
	}
	roomID, wsErr := this.enterRoom(ctx, raw)
	if wsErr != nil {
		this.replyError(ctx, conn, wsErr.Code, wsErr.Message)
		return
	}
	// JoinRoom 成功：從這一刻起，斷線清理要負責 LeaveRoom
	state.room = roomID

	this.applyJoin(ctx, conn, roomID)
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

// JoinRoom 成功後拿鎖更新 wsClient.room 並回 join_ok
func (this *WebsocketHandler) applyJoin(ctx context.Context, conn *websocket.Conn, roomID string) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	c, exists := this.clients[conn]
	if !exists {
		// 期間連線已被移除（廣播寫失敗），讀取迴圈之後會 LeaveRoom
		return
	}
	c.room = roomID
	this.writeEnvelopeLocked(ctx, conn, wsEnvelope{Type: wsTypeJoinOK, Payload: wsRoomPayload{Room: roomID}})
}

// 處理 leave frame。對 client 來說 leave 一定成功：先改 wsClient.room 並回 leave_ok，
// 再到鎖外做 LeaveRoom（失敗只記 log，由 roomSubscriptions 處理）
func (this *WebsocketHandler) handleLeave(ctx context.Context, conn *websocket.Conn, state *wsConnState) {
	if state.room == "" {
		this.replyError(ctx, conn, wsCodeNotInRoom, wsMsgNotInRoom)
		return
	}
	roomID := state.room

	this.mutex.Lock()
	if c, exists := this.clients[conn]; exists {
		c.room = ""
		this.writeEnvelopeLocked(ctx, conn, wsEnvelope{Type: wsTypeLeaveOK, Payload: wsRoomPayload{Room: roomID}})
	}
	this.mutex.Unlock()

	this.leaveRoom(ctx, roomID)
	state.room = ""
}

// 對 Redis 做 LeaveRoom（呼叫時不能持有 this.mutex），包 wsJoinTimeout
func (this *WebsocketHandler) leaveRoom(ctx context.Context, roomID string) {
	leaveCtx, cancel := context.WithTimeout(ctx, wsJoinTimeout)
	defer cancel()
	this.redis.LeaveRoom(leaveCtx, roomID)
}

// 處理 auth frame。token 解析和 DB 查詢都在鎖外做（DB 可能慢，不能擋住廣播和其他連線），
// 只有「套用身分變更 + 回 frame」才拿鎖
// 任何失敗一律降回匿名並回 ws_auth_failed（即使原本是 member），連線不關
func (this *WebsocketHandler) handleAuth(ctx context.Context, conn *websocket.Conn, raw json.RawMessage) {
	user, exp, ok := this.verifyAuthPayload(ctx, raw)

	this.mutex.Lock()
	defer this.mutex.Unlock()
	c, exists := this.clients[conn]
	if !exists {
		// 驗證期間連線已被移除（斷線 / 廣播寫失敗），沒有人可以回了
		return
	}
	if !ok {
		this.downgradeLocked(ctx, c, wsReasonAuthFailed)
		this.writeErrorLocked(ctx, conn, wsCodeAuthFailed, wsMsgAuthFailed)
		return
	}
	this.applyAuthLocked(ctx, c, user.ID, user.Username, exp)
}

// 解析 payload、驗 token、確認 user 還在。不碰鎖。
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

// token 驗證通過後套用身分（呼叫時要持有 this.mutex）：
//   - 匿名 → 該 user；member → 同一個 user = 續期；member → 不同 user = 直接換人
func (this *WebsocketHandler) applyAuthLocked(ctx context.Context, c *wsClient, userID int64, username string, exp time.Time) {
	if c.userID != userID {
		from := c.userID
		if from != 0 {
			this.memberLeftLocked(from)
		}
		c.userID = userID
		this.memberJoinedLocked(userID)
		this.logger.InfoContext(ctx, "ws identity changed", "from", identityLabel(from), "to", identityLabel(userID), "reason", wsReasonAuth)
	}
	// 同一個 user 續期不算身分變更，只更新 exp 和 timer
	c.username = username
	c.exp = exp
	this.updateViewersGaugeLocked()
	this.scheduleExpiryLocked(ctx, c)

	this.writeEnvelopeLocked(ctx, c.conn, wsEnvelope{
		Type: wsTypeAuthOK,
		Payload: wsAuthOKPayload{
			User:      userResponse{ID: userID, Username: username},
			ExpiresIn: int(time.Until(exp).Seconds()),
		},
	})
}

// 把這條連線降回匿名（呼叫時要持有 this.mutex）：停 timer、扣 refcount、記 log、更新 gauge
// 已經是匿名就什麼都不做。只改身分，不回 frame —— 回什麼 error 由呼叫端決定
func (this *WebsocketHandler) downgradeLocked(ctx context.Context, c *wsClient, reason string) {
	if c.userID == 0 {
		return
	}
	from := c.userID
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	this.memberLeftLocked(from)
	c.userID = 0
	c.username = ""
	c.exp = time.Time{}
	this.updateViewersGaugeLocked()
	this.logger.InfoContext(ctx, "ws identity changed", "from", identityLabel(from), "to", wsAnonymous, "reason", reason)
}

// 排程 / 重設「token 到期就降回匿名」的 timer（呼叫時要持有 this.mutex）
// 連線變成 member 或續期時都會呼叫
func (this *WebsocketHandler) scheduleExpiryLocked(ctx context.Context, c *wsClient) {
	// 如果本來就有timer，那就先移除
	if c.timer != nil {
		c.timer.Stop()
	}

	// 建立新的timer
	c.timer = time.AfterFunc(time.Until(c.exp), func() {
		this.mutex.Lock()
		defer this.mutex.Unlock()

		if this.clients[c.conn] != c {
			// 等lock的時候已經被其他goroutine移除了
			return
		}
		if time.Now().Before(c.exp) {
			// 等lock的時候已經被其他goroutine給renew了
			return
		}
		if c.userID == 0 {
			// 等lock的時候已經被其他goroutine降級回匿名了（例如送了壞掉的 auth frame）
			return
		}
		this.downgradeLocked(ctx, c, wsReasonExpired)
		this.writeErrorLocked(ctx, c.conn, wsCodeAuthExpired, wsMsgAuthExpired)
	})
}

// refcount +1（呼叫時要持有 this.mutex）
func (this *WebsocketHandler) memberJoinedLocked(userID int64) {
	count := this.members[userID] + 1
	this.members[userID] = count
}

// refcount -1（呼叫時要持有 this.mutex）
func (this *WebsocketHandler) memberLeftLocked(userID int64) {
	count := this.members[userID] - 1
	if count <= 0 {
		delete(this.members, userID)
	} else {
		this.members[userID] = count
	}
}

// member = 不重複 user 數（len(members)）；anonymous = 匿名連線數（直接數 clients，最多 maxClients 條）
func (this *WebsocketHandler) updateViewersGaugeLocked() {
	anonymous := 0
	for _, c := range this.clients {
		if c.userID == 0 {
			anonymous++
		}
	}
	prom.WebsocketViewers.WithLabelValues("member").Set(float64(len(this.members)))
	prom.WebsocketViewers.WithLabelValues("anonymous").Set(float64(anonymous))
}

func identityLabel(userID int64) string {
	if userID == 0 {
		return wsAnonymous
	}
	return strconv.FormatInt(userID, 10)
}

// 回一個 error envelope 給這條連線（呼叫時要持有 this.mutex）
func (this *WebsocketHandler) writeErrorLocked(ctx context.Context, conn *websocket.Conn, code, message string) {
	this.writeEnvelopeLocked(ctx, conn, wsEnvelope{
		Type:    wsTypeError,
		Payload: wsError{Code: code, Message: message},
	})
}

// 寫一個 envelope 給這條連線（呼叫時要持有 this.mutex）
// gorilla 同一條連線同時只能有一個 writer；StartBroadcast 也是在 this.mutex 下寫所有 client，
// 所以寫之前一定要拿同一把鎖，不然會跟廣播同時寫同一條連線
// 已知問題（W6 Hub 重構時處理，見 ROADMAP）：這把是全域鎖，一個慢的 client 寫入卡住會連帶擋住廣播
func (this *WebsocketHandler) writeEnvelopeLocked(ctx context.Context, conn *websocket.Conn, env wsEnvelope) {
	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		this.logger.WarnContext(ctx, "set write deadline failed", "error", err)
		return
	}
	// 寫失敗只記 log：連線真的壞了，下一次 ReadMessage 會失敗並走 removeClient
	if err := conn.WriteJSON(env); err != nil {
		this.logger.WarnContext(ctx, "write envelope failed", "error", err, "type", env.Type)
	}
}

func (this *WebsocketHandler) StartBroadcast() {
	// Redis subscribe 是 server 層級的長期任務，不屬於任何 request
	// 所以要用context.Background()
	this.redis.Subscribe(context.Background(), func(roomID, msg string) {
		this.mutex.Lock()
		defer this.mutex.Unlock()
		this.broadcastToRoomLocked(roomID, msg)
	})
}

// 把 msg 送給所有在 roomID 的連線（呼叫時要持有 this.mutex）
// 命名慣例：結尾是 Locked 的 function 都要求呼叫端已經持有 this.mutex
func (this *WebsocketHandler) broadcastToRoomLocked(roomID, msg string) {
	for conn, c := range this.clients {
		// 只送給在這個房間的連線
		if c.room != roomID {
			continue
		}
		if err := this.writeRawLocked(conn, []byte(msg)); err != nil {
			// 寫失敗：為了避免有機率影響到其他用戶，移除該 client（其他 client 照送）
			conn.Close()
			this.removeClientLocked(conn)
			continue
		}
	}
}

// 寫一則原始文字訊息給這條連線（呼叫時要持有 this.mutex）
// SetWriteDeadline 或 WriteMessage 任一失敗就回傳 error，由呼叫端負責 Close + removeClientLocked
func (this *WebsocketHandler) writeRawLocked(conn *websocket.Conn, msg []byte) error {
	maxWaitingTime := time.Now().Add(writeWait)
	if err := conn.SetWriteDeadline(maxWaitingTime); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// 新連線一律是匿名：這裡只加進 clients、總連線數 +1，refcount 不動（auth 成功才 memberJoinedLocked）
//
// TODO(W6): clients / members 都是這個 process 自己記憶體裡的 map，只算得到「這台 server」的連線
// 同一個 user 一條連 server-1、一條連 server-2 → 兩台各回報 member=1，Grafana 加總變 2（其實 1 人）
// 跨 server 去重要放到共用的地方（Redis）+ TTL + heartbeat：server 被 kill -9 時沒機會清，
// 只能靠「活著的定期續命、死掉的自然過期」，見 ROADMAP W6「跨 server 的不同會員數」
func (this *WebsocketHandler) addClientLocked(conn *websocket.Conn) {
	this.clients[conn] = &wsClient{conn: conn}
	prom.WebsocketConnections.Inc()
	this.updateViewersGaugeLocked()
}

// 斷線：停 timer、是會員就 refcount -1、總連線數 -1；重複呼叫不會重複扣（廣播寫失敗和讀取失敗都可能呼叫）
//
// TODO(W6): 同 addClientLocked，只扣得到這台 server 的數字；process 直接死掉時這裡根本不會執行，
// 換成 Redis 版本後要靠 TTL 清掉死掉那台留下的資料
func (this *WebsocketHandler) removeClientLocked(conn *websocket.Conn) {
	c, ok := this.clients[conn]
	if !ok {
		// already removed, might be remove from other goroutine
		return
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.userID != 0 {
		this.memberLeftLocked(c.userID)
		c.userID = 0
	}
	prom.WebsocketConnections.Dec()
	delete(this.clients, conn)
	this.updateViewersGaugeLocked()
}
