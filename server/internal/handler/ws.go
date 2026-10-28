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
	wsTypeAuth      = "auth"    // client → server：連線內登入 / 續期
	wsTypeAuthOK    = "auth_ok" // server → client：登入 / 續期成功

	wsCodeWriteForbidden = "ws_write_forbidden"
	wsMsgWriteForbidden  = "請透過 POST /messages 發送"
	wsCodeAuthFailed     = "ws_auth_failed"
	wsMsgAuthFailed      = "登入驗證失敗，目前為訪客"
	wsCodeAuthExpired    = "ws_auth_expired"
	wsMsgAuthExpired     = "登入已過期，目前為訪客"

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
}

// 這個檔案裡的 3 種 context：
//
//	StartBroadcast        → context.Background()  process 生命週期，跟任何 request 無關
//	HandleWebsocket       → r.Context()           這條 WS 連線的生命週期，帶 request_id
//	  └ listenConnection / removeClient → 沿用上層 ctx
//	    └ processRemoteMessage → 沿用上層 ctx
//	      └ handleAuth → 沿用上層 ctx，但 GetUserByID 另外包 wsAuthDBTimeout（DB 卡住不能擋住讀取迴圈）
//	      其餘（回 error frame）沒有 DB / Redis 呼叫，不需要 timeout
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
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// constuctor
func NewWebsocketHandler(redis store.RedisStoreInterface, db store.DBStoreInterface, tokens AccessTokenClaimsParser) *WebsocketHandler {
	return &WebsocketHandler{
		redis:   redis,
		db:      db,
		tokens:  tokens,
		clients: make(map[*websocket.Conn]*wsClient),
		members: make(map[int64]int),
		logger:  slog.Default().With("component", "websocket"),
	}
}

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
	connection, err := upgrader.Upgrade(w, r, w.Header())
	if err != nil {
		this.logger.ErrorContext(r.Context(), "ws upgrade failed", "error", err)
		return
	}
	defer connection.Close()

	if !this.tryAddClient(r.Context(), connection) {
		return
	}
	// 連線成功，設定每次發送的上限
	connection.SetReadLimit(maxMessageBytes)
	this.listenConnection(r.Context(), connection)
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

func (this *WebsocketHandler) listenConnection(ctx context.Context, connection *websocket.Conn) {
	var msgCount int
	windowStart := time.Now()

	for {
		_, msg, err := connection.ReadMessage()
		if err != nil {
			// connection遇到錯誤, 把該connection移除
			this.removeClient(ctx, connection)
			break
		}
		// auth frame 裡有 access token，不能進 log；誰送的看身分變化的 INFO log
		this.logger.DebugContext(ctx, "info received", "bytes", len(msg))
		// 加上一秒內5則的rate limit
		now := time.Now()
		if now.Sub(windowStart) > time.Second {
			msgCount = 0
			windowStart = now
		}
		msgCount++
		if msgCount > rateLimit {
			this.logger.WarnContext(ctx, "too many msgs sent", "limit", rateLimit)
			continue
		}

		this.processRemoteMessage(ctx, connection, msg)
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
//   - 其他（含不是合法 JSON）→ 回 ws_write_forbidden
//
// WS 不能寫入：發訊息要走 POST /messages（那裡才驗得了身分、存得進 DB 並帶 user_id）
// 這裡只回一個 error envelope 給「這條連線」，不存 DB、不 Publish、不關連線
// 目前對所有人一律拒絕，直到 W4b 有角色（房主 / curator）才會有人能透過 WS 寫
//
// 每條連線的 5 則/秒限流（listenConnection）同時也限制了 error frame 的數量（auth frame 也算），
// 不用另外擋「狂送訊息逼 server 狂回錯誤」
func (this *WebsocketHandler) processRemoteMessage(ctx context.Context, connection *websocket.Conn, msg []byte) {
	var in struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg, &in); err != nil || in.Type != wsTypeAuth {
		this.mutex.Lock()
		defer this.mutex.Unlock()
		this.writeErrorLocked(ctx, connection, wsCodeWriteForbidden, wsMsgWriteForbidden)
		return
	}
	this.handleAuth(ctx, connection, in.Payload)
}

// 處理 auth frame。token 解析和 DB 查詢都在鎖外做（DB 可能慢，不能擋住廣播和其他連線），
// 只有「套用身分變更 + 回 frame」才拿鎖
// 任何失敗一律降回匿名並回 ws_auth_failed（即使原本是 member），連線不關
func (this *WebsocketHandler) handleAuth(ctx context.Context, connection *websocket.Conn, raw json.RawMessage) {
	user, exp, ok := this.verifyAuthPayload(ctx, raw)

	this.mutex.Lock()
	defer this.mutex.Unlock()
	c, exists := this.clients[connection]
	if !exists {
		// 驗證期間連線已被移除（斷線 / 廣播寫失敗），沒有人可以回了
		return
	}
	if !ok {
		this.downgradeLocked(ctx, c, wsReasonAuthFailed)
		this.writeErrorLocked(ctx, connection, wsCodeAuthFailed, wsMsgAuthFailed)
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

// 用 Background() 是對的：Redis subscribe 是 server 層級的長期任務，不屬於任何 request
func (this *WebsocketHandler) StartBroadcast() {
	this.redis.Subscribe(context.Background(), func(msg string) {
		this.mutex.Lock()
		defer this.mutex.Unlock()
		for client := range this.clients {
			maxWaitingTime := time.Now().Add(writeWait)
			if err := client.SetWriteDeadline(maxWaitingTime); err != nil {
				// 為了避免有機率影響到其他用戶，移除該client
				client.Close()
				this.removeClientLocked(client)
				continue
			}
			if err := client.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				/// for loop check other client is alive, if not
				/// remove it from client list due to write failed
				client.Close()
				this.removeClientLocked(client)
				continue
			}
		}
	})
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
