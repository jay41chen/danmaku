package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/config"
	"github.com/jay41chen/danmaku/server/internal/handler"
	logging "github.com/jay41chen/danmaku/server/internal/logging"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const accessTokenTTL = time.Hour

func main() {
	initApp()
	// 設定：只讀 env + 檢查，沒有副作用；設錯就在這裡停，不會先連 DB、跑 migration
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	logClientIPHeader(cfg.ClientIPHeader)
	tokens := initTokenIssuer(cfg.JWTSecret)

	db := initDatabase(cfg.DatabaseURL)
	redis := initRedis(cfg.RedisURL)

	// RedisStore 同時實作 Pub/Sub 和 RateLimiterInterface，同一條連線共用
	wsHandler := handler.NewWebsocketHandler(redis, db, tokens)
	mux := http.NewServeMux()
	registerRoutes(mux, tokens, appHandlers{
		messages: handler.NewMessageHandler(db, redis, redis),
		health:   handler.NewHealthHandler(db, redis),
		auth:     handler.NewAuthHandler(db, redis, cfg.ClientIPHeader, tokens),
		ws:       wsHandler,
	})
	go wsHandler.StartBroadcast()

	startServer(mux, cfg.Port, cfg.CORSOrigins)
}

func initApp() {
	// init slog
	jsonHandler := slog.NewJSONHandler(os.Stdout, nil)
	contextHandler := logging.NewContextHandler(jsonHandler)
	slog.SetDefault(slog.New(contextHandler))
}

func initDatabase(dbURL string) *store.DBStore {
	// 這邊隱含了semantic上的順序，先準備好schema再建立DB
	if err := store.RunMigration(dbURL); err != nil {
		slog.Error("db migration failed", "error", err)
		os.Exit(1)
	}
	dbStore, err := store.NewDBStore(context.Background(), dbURL)
	if err != nil {
		// fatal will terminate the execution!!
		slog.Error("db init failed", "error", err)
		os.Exit(1)
	}

	return dbStore
}

func initRedis(redisURL string) *store.RedisStore {
	redisStore, err := store.NewRedisStore(context.Background(), redisURL)
	if err != nil {
		slog.Error("redis init failed", "error", err)
		os.Exit(1)
	}
	return redisStore
}

// 限流要用的 client IP 從哪裡讀。只能設成「最前面那層 trusted proxy 會覆蓋」的 header，
// 否則 client 可以自己偽造（docker-compose：nginx；Railway：edge proxy，兩邊都是 X-Real-IP）
// 沒設不算設定錯誤（config 不擋），只在啟動時提醒
func logClientIPHeader(header string) {
	if header == "" {
		// 不 fail fast：本機直接 go run（前面沒有 proxy）時 RemoteAddr 就是對的
		// 但在 proxy 後面忘了設，RemoteAddr 會是 proxy 的 IP，所有人共用同一個限流計數
		slog.Warn("CLIENT_IP_HEADER not set, rate limiting uses RemoteAddr; behind a proxy all clients share one limit")
		return
	}
	slog.Info("client ip from header", "header", header)
}

// JWT_SECRET 沒有安全的預設值（寫死 = 進 git 誰都能簽；隨機 = 兩台 server 互不認、每次部署全部登出）
// → config.Load 已經 fail fast 檢查過長度；這裡的 error 理論上不會發生，保留是因為 NewTokenIssuer 自己也守不變式
func initTokenIssuer(secret string) *auth.TokenIssuer {
	tokens, err := auth.NewTokenIssuer(secret, accessTokenTTL)
	if err != nil {
		// error 只帶長度，不帶 secret 本身
		slog.Error("token issuer init failed", "error", err)
		os.Exit(1)
	}
	return tokens
}

// 所有 route 用到的 handler，集中在一起，registerRoutes 才好在 test 裡換成假的依賴
type appHandlers struct {
	messages *handler.MessageHandler
	health   *handler.HealthHandler
	auth     *handler.AuthHandler
	ws       *handler.WebsocketHandler
}

// route 註冊的唯一入口：每條 route 要嘛 get / post（要登入），要嘛 publicGet / publicPost（公開）
// 預設 = 要登入（fail closed）：忘了想這件事時，結果是「多擋了人」而不是「多開了洞」
// 公開要明寫 public，code review 看得到；如果預設是公開，少寫一個 middleware 在 diff 裡是看不出來的
type routes struct {
	mux  *http.ServeMux
	auth func(http.Handler) http.Handler
}

func (this *routes) get(path string, h http.Handler) { this.handle(http.MethodGet, path, this.auth(h)) }
func (this *routes) post(path string, h http.Handler) {
	this.handle(http.MethodPost, path, this.auth(h))
}
func (this *routes) publicGet(path string, h http.Handler) {
	this.handle(http.MethodGet, path, h)
}
func (this *routes) publicPost(path string, h http.Handler) {
	this.handle(http.MethodPost, path, h)
}

// Go 1.22 的 "METHOD /path" pattern：method 不符由 mux 自己回 405（附 Allow header），
// handler 裡不用再各自檢查 method
// 注意 "GET /x" 也會比對 HEAD（healthcheck 原本就是 GET + HEAD 都允許，行為不變）
// CORS preflight（OPTIONS）不會走到這裡：CORS middleware 包在 mux 外面，會先回 204
func (this *routes) handle(method, path string, h http.Handler) {
	this.mux.Handle(method+" "+path, h)
}

func registerRoutes(mux *http.ServeMux, tokens middleware.AccessTokenParser, h appHandlers) {
	r := &routes{mux: mux, auth: middleware.RequireAuth(tokens)}

	// 同一個 path 的 GET 是公開的、POST 要登入：讀歷史訊息不用登入，發訊息要
	r.publicGet("/messages", http.HandlerFunc(h.messages.HandleGetMessages))
	r.post("/messages", http.HandlerFunc(h.messages.HandleCreateMessage))

	r.publicGet("/healthcheck", http.HandlerFunc(h.health.HandleHealthCheck))
	// 公開 route：任何人都能連（匿名）；連上後可以在連線內送 auth frame 升級成登入身分（選擇性）
	// WS 不接受寫入（發訊息走 POST /messages），auth 失敗也不會斷線
	r.publicGet("/ws", http.HandlerFunc(h.ws.HandleWebsocket))
	// 如果要custom registry再換
	r.publicGet("/metrics", promhttp.Handler())

	r.publicPost("/register", http.HandlerFunc(h.auth.HandleRegister))
	r.publicPost("/login", http.HandlerFunc(h.auth.HandleLogin))
	r.publicPost("/refresh", http.HandlerFunc(h.auth.HandleRefresh))
	r.publicPost("/logout", http.HandlerFunc(h.auth.HandleLogout))
	// 要登入：回目前 token 對應的 user
	r.get("/me", http.HandlerFunc(h.auth.HandleMe))
}

// origins 已經由 config.Load 用 middleware.ParseOrigins 解析、檢查過
func startServer(mux *http.ServeMux, port string, origins []string) {

	// 建立SERVER
	server := http.Server{
		Addr: ":" + port,
		Handler: middleware.Chain(
			mux,
			middleware.RequestID,
			middleware.Logging(slog.Default()),
			prom.HttpMetrics,
			middleware.RecoverPanic,
			middleware.CORS(origins...),
		),
		// net/http 執行期間自己的錯誤（外層 panic、重複 WriteHeader、TLS/accept 失敗）走這裡，
		// middleware 攔不到。不設的話會經由 slog.SetDefault 以 INFO 輸出，這裡改成 ERROR
		ErrorLog: slog.NewLogLogger(
			slog.Default().With("component", "http_server").Handler(),
			slog.LevelError),
	}

	// 監聽 SIGTERM（docker stop / Railway deploy）和 SIGINT（Ctrl+C）
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// goroutine for server running state
	// for receive application service ex: api/ws request
	go func() {
		slog.Info("server listening", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()
	// main rountine suspend untill receive ctxDone,
	<-ctx.Done()
	slog.Info("shutting down server")

	// 給 10 秒讓正在處理的 request 完成
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// server.Shutdown => graceful shutdown,
	// 會關閉tcp連線，等現有連線處理完或是timeout
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped gracefully")
}
