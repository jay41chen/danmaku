package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"
)

const (
	maxClients     = 100
	maxMessageSize = 500
	rateLimit      = 5
)

type WebsocketHandler struct {
	redis   store.RedisStoreInterface
	db      store.DBStoreInterface
	clients map[*websocket.Conn]bool
	mutex   sync.Mutex
	logger  *slog.Logger
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// constuctor
func NewWebsocketHandler(redis store.RedisStoreInterface, db store.DBStoreInterface) *WebsocketHandler {
	return &WebsocketHandler{
		redis:   redis,
		db:      db,
		clients: make(map[*websocket.Conn]bool),
		logger:  slog.Default().With("component", "websocket"),
	}
}

func (this *WebsocketHandler) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	// 先檢查上限
	this.mutex.Lock()
	err := this.checkClientLimit(w)
	if err != nil {
		this.mutex.Unlock()
		return
	}
	this.mutex.Unlock()
	// WebSocket 連線的起點其實是一個普通的 HTTP request。
	// 瀏覽器先發一個 HTTP request 說「我想升級成 WebSocket」，
	// server 同意後才切換協定。
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		this.logger.Error("ws upgrade failed", "error", err)
		return
	}
	defer connection.Close()

	this.addClient(connection)
	this.listenConnection(connection)
}

func (this *WebsocketHandler) checkClientLimit(w http.ResponseWriter) error {
	if len(this.clients) >= maxClients {
		// ??? 不懂為什麼http.Error是void
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return fmt.Errorf("too many connections")
	}
	return nil
}

func (this *WebsocketHandler) listenConnection(connection *websocket.Conn) {
	var msgCount int
	windowStart := time.Now()

	for {
		_, msg, err := connection.ReadMessage()
		if err != nil {
			// connection遇到錯誤, 把該connection移除
			this.removeClient(connection)
			break
		}
		this.logger.Debug("info received", "msg", string(msg))
		// 加上一秒內5則的rate limit
		now := time.Now()
		if now.Sub(windowStart) > time.Second {
			msgCount = 0
			windowStart = now
		}
		msgCount++
		if msgCount > rateLimit {
			this.logger.Warn("too many msgs sent", "limit", rateLimit)
			continue
		}

		this.processRemoteMessage(string(msg))
	}

}

func (this *WebsocketHandler) addClient(conn *websocket.Conn) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.addClientLocked(conn)
	/// 有辦法找到conn的uuid之類的嗎
	this.logger.Info("new client connected", "total", len(this.clients))
}

func (this *WebsocketHandler) removeClient(conn *websocket.Conn) {
	this.mutex.Lock()
	defer this.mutex.Unlock()
	this.removeClientLocked(conn)
	this.logger.Info("client disconnected", "total", len(this.clients))
}

func (this *WebsocketHandler) processRemoteMessage(msg string) {
	ctx := context.Background()
	// 檢查是否可以寫入DB
	if err := this.db.SaveMessage(ctx, msg); err != nil {
		this.logger.Error("save message to db failed", "error", err)
	}
	// 檢查是否可以發到redis
	if err := this.redis.Publish(ctx, msg); err != nil {
		this.logger.Error("publish to redis failed", "error", err)
	}
}

func (this *WebsocketHandler) StartBroadcast() {
	this.redis.Subscribe(context.Background(), func(msg string) {
		this.mutex.Lock()
		for client := range this.clients {
			if err := client.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				/// for loop check other client is alive, if not
				/// remove it from client list
				client.Close()
				this.removeClientLocked(client)
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
	prom.WebsocketConnections.Dec()
	delete(this.clients, conn)
}
