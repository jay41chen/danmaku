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
	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/loadtiming"
	logging "github.com/jay41chen/danmaku/server/internal/logging"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/pgstore"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/redisstore"

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
	logLoadtestSettings(cfg)
	tokens := initTokenIssuer(cfg.JWTSecret)

	db := initDatabase(cfg.DatabaseURL)
	redis := initRedis(cfg.RedisURL)

	// RedisStore 同時實作 Pub/Sub 和 rateLimiter，同一條連線共用
	wsHandler := handler.NewWebsocketHandler(redis, db, tokens, handler.WithMaxClients(cfg.WSMaxClients))
	msgDeps := messageDepsFor(db, redis, cfg.LoadtestTimingLog)
	messageHandler := handler.NewMessageHandler(msgDeps.store, msgDeps.publisher, msgDeps.limiter)
	mux := http.NewServeMux()
	registerRoutes(mux, tokens, appHandlers{
		messages: messageHandler,
		rooms:    handler.NewRoomHandler(db),
		roles:    handler.NewRoleHandler(db, redis),
		health:   handler.NewHealthHandler(db, redis),
		auth:     handler.NewAuthHandler(db, redis, cfg.ClientIPHeader, tokens),
		ws:       wsHandler,
	})
	go wsHandler.StartBroadcast()
	go wsHandler.StartPresence(redis)

	startServer(mux, cfg.Port, cfg.CORSOrigins)
}

// 訊息 handler 需要的三個依賴；型別是 handler 那邊 messageStore / messagePublisher / rateLimiter 的超集合，
// 所以可以直接傳給 NewMessageHandler
type messageStoreAPI interface {
	CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, bool, error)
	QueryMessage(ctx context.Context, roomID int64, limit int) ([]model.Message, error)
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)
}

type messagePublisherAPI interface {
	Publish(ctx context.Context, roomID, msg string) error
}

type messageLimiterAPI interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error)
	Reset(ctx context.Context, key string) error
	Refund(ctx context.Context, key string) error
}

type messageDeps struct {
	store     messageStoreAPI
	publisher messagePublisherAPI
	limiter   messageLimiterAPI
}

// 壓測用的計時：LOADTEST_TIMING_LOG 沒開時原封不動回傳 db / redis（沒有 wrapper、沒有背景 goroutine）；
// 開了才用 wrapper 包住 CreateMessage、限流 Allow、Redis PUBLISH 各自計時，並啟動每 5 秒 log 一行的 goroutine
func messageDepsFor(db *pgstore.DBStore, redis *redisstore.RedisStore, timingLog bool) messageDeps {
	if !timingLog {
		return messageDeps{store: db, publisher: redis, limiter: redis}
	}
	slog.Warn("LOADTEST_TIMING_LOG on: logging DB / limiter / publish timing every 5s (load test only)")
	hists := loadtiming.Hists{Create: loadtiming.NewHistogram(), Limiter: loadtiming.NewHistogram(), Publish: loadtiming.NewHistogram()}
	go loadtiming.Run(context.Background(), slog.Default().With("component", "loadtiming"), loadtiming.DefaultInterval, hists,
		func() loadtiming.PoolSample {
			s := db.PoolStat()
			return loadtiming.PoolSample{
				AcquireCount:         s.AcquireCount(),
				AcquireDuration:      s.AcquireDuration(),
				EmptyAcquireCount:    s.EmptyAcquireCount(),
				CanceledAcquireCount: s.CanceledAcquireCount(),
				Acquired:             s.AcquiredConns(),
				Idle:                 s.IdleConns(),
				Max:                  s.MaxConns(),
			}
		})
	return messageDeps{
		store:     timedStore{DBStore: db, hist: hists.Create},
		publisher: timedPublisher{messagePublisherAPI: redis, hist: hists.Publish},
		limiter:   timedLimiter{messageLimiterAPI: redis, hist: hists.Limiter},
	}
}

// 只覆寫 CreateMessage 計時，其餘方法沿用內嵌的 *pgstore.DBStore
type timedStore struct {
	*pgstore.DBStore
	hist *loadtiming.Histogram
}

func (s timedStore) CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, bool, error) {
	start := time.Now()
	msg, banned, err := s.DBStore.CreateMessage(ctx, roomPublicID, userID, content)
	s.hist.Observe(time.Since(start), err != nil)
	return msg, banned, err
}

// 只覆寫 Allow 計時（POST 每次問兩條規則，所以每個 POST 記兩筆），Reset / Refund 沿用內嵌的 limiter
type timedLimiter struct {
	messageLimiterAPI
	hist *loadtiming.Histogram
}

func (l timedLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	start := time.Now()
	allowed, retryAfter, err := l.messageLimiterAPI.Allow(ctx, key, limit, window)
	l.hist.Observe(time.Since(start), err != nil)
	return allowed, retryAfter, err
}

// 只覆寫 Publish 計時
type timedPublisher struct {
	messagePublisherAPI
	hist *loadtiming.Histogram
}

func (p timedPublisher) Publish(ctx context.Context, roomID, msg string) error {
	start := time.Now()
	err := p.messagePublisherAPI.Publish(ctx, roomID, msg)
	p.hist.Observe(time.Since(start), err != nil)
	return err
}

// 壓測用的設定一旦偏離 production 預設就在啟動時 log 出來，免得測完忘了、或拿壓測的 server 當成正常設定
func logLoadtestSettings(cfg config.Config) {
	if cfg.WSMaxClients != config.DefaultWSMaxClients {
		slog.Warn("non-default load test settings", "ws_max_clients", cfg.WSMaxClients)
	}
}

func initApp() {
	// init slog
	jsonHandler := slog.NewJSONHandler(os.Stdout, nil)
	contextHandler := logging.NewContextHandler(jsonHandler)
	slog.SetDefault(slog.New(contextHandler))
}

func initDatabase(dbURL string) *pgstore.DBStore {
	// 這邊隱含了semantic上的順序，先準備好schema再建立DB
	if err := pgstore.RunMigration(dbURL); err != nil {
		slog.Error("db migration failed", "error", err)
		os.Exit(1)
	}
	dbStore, err := pgstore.NewDBStore(context.Background(), dbURL)
	if err != nil {
		// fatal will terminate the execution!!
		slog.Error("db init failed", "error", err)
		os.Exit(1)
	}

	return dbStore
}

func initRedis(redisURL string) *redisstore.RedisStore {
	redisStore, err := redisstore.NewRedisStore(context.Background(), redisURL)
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
	rooms    *handler.RoomHandler
	roles    *handler.RoleHandler
	health   *handler.HealthHandler
	auth     *handler.AuthHandler
	ws       *handler.WebsocketHandler
}

// route 註冊的唯一入口：每條 route 要嘛 get / post（要登入），要嘛 publicGet / publicPost（公開），要嘛 removed（已下架，回 410）
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
func (this *routes) put(path string, h http.Handler) { this.handle(http.MethodPut, path, this.auth(h)) }
func (this *routes) delete(path string, h http.Handler) {
	this.handle(http.MethodDelete, path, this.auth(h))
}
func (this *routes) publicGet(path string, h http.Handler) {
	this.handle(http.MethodGet, path, h)
}
func (this *routes) publicPost(path string, h http.Handler) {
	this.handle(http.MethodPost, path, h)
}

// 已移除的 route：任何 method 都回 410 Gone + 指路到 replacement
// 刻意不收 handler：只能拿來下架，沒辦法被拿去開「公開、不分 method」的 API（繞過上面的 fail closed）
// 公開：不讀不寫任何資料；如果要登入，沒帶 token 的舊 client 只會拿到 401、以為要去 /refresh，永遠不知道 API 沒了
// pattern 不帶 METHOD，所有 method 都比對得到（CORS preflight 一樣先被外層 CORS middleware 回掉）
func (this *routes) removed(path, replacement string) {
	this.mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusGone, "gone", "gone: use "+replacement)
	}))
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
	r.publicGet("/rooms/{public_id}/messages", http.HandlerFunc(h.messages.HandleGetRoomMessages))
	r.post("/rooms/{public_id}/messages", http.HandlerFunc(h.messages.HandleCreateRoomMessage))
	// 舊的 legacy 房間 API，前端已改用上面的 /rooms/{public_id}/messages
	r.removed("/messages", "/rooms/{public_id}/messages")

	r.post("/rooms", http.HandlerFunc(h.rooms.HandleCreateRoom))
	r.publicGet("/rooms/{public_id}", http.HandlerFunc(h.rooms.HandleGetRoom))
	// 公開：列出所有人的房間（分頁）
	r.publicGet("/rooms", http.HandlerFunc(h.rooms.HandleListRooms))

	// 房間角色（房管 / 房間級 shadow ban）：都要登入，權限由 store 的 transaction 判斷
	r.put("/rooms/{public_id}/roles/{user_id}", http.HandlerFunc(h.roles.HandleSetRole))
	r.delete("/rooms/{public_id}/roles/{user_id}", http.HandlerFunc(h.roles.HandleDeleteRole))
	r.get("/rooms/{public_id}/roles", http.HandlerFunc(h.roles.HandleListRoles))
	r.get("/rooms/{public_id}/me", http.HandlerFunc(h.roles.HandleMyRole))

	r.publicGet("/healthcheck", http.HandlerFunc(h.health.HandleHealthCheck))
	// 公開 route：任何人都能連（匿名）；連上後可以在連線內送 auth frame 升級成登入身分（選擇性）
	// WS 不接受寫入（發訊息走 POST /rooms/{public_id}/messages），auth 失敗也不會斷線
	r.publicGet("/ws", http.HandlerFunc(h.ws.HandleWebsocket))
	// 如果要custom registry再換
	r.publicGet("/metrics", promhttp.Handler())

	r.publicPost("/register", http.HandlerFunc(h.auth.HandleRegister))
	r.publicPost("/login", http.HandlerFunc(h.auth.HandleLogin))
	r.publicPost("/refresh", http.HandlerFunc(h.auth.HandleRefresh))
	r.publicPost("/logout", http.HandlerFunc(h.auth.HandleLogout))
	// 要登入：回目前 token 對應的 user
	r.get("/me", http.HandlerFunc(h.auth.HandleMe))
	// 要登入：列出自己的房間（分頁）
	r.get("/me/rooms", http.HandlerFunc(h.rooms.HandleListMyRooms))
}

// prod 和 test 共用的 middleware chain：test 走這裡，才測得到跟 prod 一樣的順序
// 順序限制：HttpMetrics 和 mux 中間不能有會複製 request 的 middleware（r.WithContext，例如 RequestID）
// mux 是把 r.Pattern 寫進「它拿到的那個 *Request」，中間被複製的話 HttpMetrics 讀到的是 ""，全部變成 unmatched
func serviceHandler(mux *http.ServeMux, origins []string) http.Handler {
	return middleware.Chain(
		mux,
		middleware.RequestID,
		middleware.Logging(slog.Default()),
		prom.HttpMetrics,
		middleware.RecoverPanic,
		middleware.CORS(origins...),
	)
}

// origins 已經由 config.Load 用 middleware.ParseOrigins 解析、檢查過
func startServer(mux *http.ServeMux, port string, origins []string) {

	// 建立SERVER
	server := http.Server{
		Addr:    ":" + port,
		Handler: serviceHandler(mux, origins),
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
