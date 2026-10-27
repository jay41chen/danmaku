package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"
)

const (
	maxClients      = 100
	maxMessageBytes = 500
	rateLimit       = 5
	writeWait       = 10 * time.Second
)

// WS 上跑的訊息都包成 envelope：{"type": ..., "payload": ...}
// client 靠 type 分辨收到的是廣播的訊息還是錯誤，之後要加新種類（presence 等）也不用改格式
const (
	wsTypeMessage = "message"
	wsTypeError   = "error"

	wsCodeWriteForbidden = "ws_write_forbidden"
	wsMsgWriteForbidden  = "請透過 POST /messages 發送"
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

// 這個檔案裡的 3 種 context：
//
//	StartBroadcast        → context.Background()  process 生命週期，跟任何 request 無關
//	HandleWebsocket       → r.Context()           這條 WS 連線的生命週期，帶 request_id
//	  └ listenConnection / removeClient → 沿用上層 ctx
//	    └ processRemoteMessage → 沿用上層 ctx（只回錯誤給這條連線，沒有 DB / Redis 呼叫，不需要 timeout）
//
// ctx 負責兩件事：1. 帶資料（request_id）2. 控制生命週期（取消、deadline）
// Background() 兩者都沒有：log 不會有 request_id，DB/Redis 卡住時也會永遠等下去
type WebsocketHandler struct {
	redis   store.RedisStoreInterface
	clients map[*websocket.Conn]bool
	mutex   sync.Mutex
	logger  *slog.Logger
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// constuctor
func NewWebsocketHandler(redis store.RedisStoreInterface) *WebsocketHandler {
	return &WebsocketHandler{
		redis:   redis,
		clients: make(map[*websocket.Conn]bool),
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

	this.mutex.Lock()
	if len(this.clients) >= maxClients {
		this.logger.WarnContext(r.Context(), "too many connections")
		this.mutex.Unlock()
		return
	}
	this.addClientLocked(connection)
	// request_id 在 upgrade 時產生，同一條連線的所有 log 共用，等同 connection ID
	this.logger.InfoContext(r.Context(), "new client connected", "total", len(this.clients))
	this.mutex.Unlock()
	// 連線成功，設定每次發送的上限
	connection.SetReadLimit(maxMessageBytes)
	this.listenConnection(r.Context(), connection)
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
		this.logger.DebugContext(ctx, "info received", "content", string(msg))
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

		this.processRemoteMessage(ctx, connection)
	}

}

func (this *WebsocketHandler) removeClient(ctx context.Context, conn *websocket.Conn) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.removeClientLocked(conn)
	this.logger.InfoContext(ctx, "client disconnected", "total", len(this.clients))
}

// WS 不能寫入：發訊息要走 POST /messages（那裡才驗得了身分、存得進 DB 並帶 user_id）
// 這裡只回一個 error envelope 給「這條連線」，不存 DB、不 Publish、不關連線
// 目前對所有人一律拒絕，直到 W4b 有角色（房主 / curator）才會有人能透過 WS 寫
//
// 每條連線的 5 則/秒限流（listenConnection）同時也限制了 error frame 的數量，
// 不用另外擋「狂送訊息逼 server 狂回錯誤」
func (this *WebsocketHandler) processRemoteMessage(ctx context.Context, connection *websocket.Conn) {
	env := wsEnvelope{
		Type:    wsTypeError,
		Payload: wsError{Code: wsCodeWriteForbidden, Message: wsMsgWriteForbidden},
	}

	// gorilla 同一條連線同時只能有一個 writer；StartBroadcast 也是在 this.mutex 下寫所有 client，
	// 所以這裡要拿同一把鎖，不然會跟廣播同時寫同一條連線
	// 已知問題（W6 Hub 重構時處理，見 ROADMAP）：這把是全域鎖，一個慢的 client 寫入卡住會連帶擋住廣播
	this.mutex.Lock()
	defer this.mutex.Unlock()
	if err := connection.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		this.logger.WarnContext(ctx, "set write deadline failed", "error", err)
		return
	}
	// 寫失敗只記 log：連線真的壞了，下一次 ReadMessage 會失敗並走 removeClient
	if err := connection.WriteJSON(env); err != nil {
		this.logger.WarnContext(ctx, "write error envelope failed", "error", err)
	}
}

// 用 Background() 是對的：Redis subscribe 是 server 層級的長期任務，不屬於任何 request
func (this *WebsocketHandler) StartBroadcast() {
	this.redis.Subscribe(context.Background(), func(msg string) {
		this.mutex.Lock()
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
		this.mutex.Unlock()
	})
}

func (this *WebsocketHandler) addClientLocked(conn *websocket.Conn) {
	this.clients[conn] = true
	prom.WebsocketConnections.Inc()
}
func (this *WebsocketHandler) removeClientLocked(conn *websocket.Conn) {
	_, ok := this.clients[conn]
	if !ok {
		// already removed, might be remove from other goroutine
		return
	}
	prom.WebsocketConnections.Dec()
	delete(this.clients, conn)
}
