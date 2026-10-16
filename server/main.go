package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jay41chen/danmaku/server/internal/handler"
	logging "github.com/jay41chen/danmaku/server/internal/logging"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
	"github.com/jay41chen/danmaku/server/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	initApp()
	registerPrometheus()
	db := initDatabase()
	redis := initRedis()

	apiHandler := handler.NewAPIHandler(db, redis)
	registerAPIRoute(apiHandler)

	wsHandler := handler.NewWebsocketHandler(redis, db)
	registerWSRoute(wsHandler)
	go wsHandler.StartBroadcast()

	startServer()
}

func registerPrometheus() {
	// 如果要custom registry再換
	http.Handle("/metrics", promhttp.Handler())
}

func initApp() {
	// init slog
	jsonHandler := slog.NewJSONHandler(os.Stdout, nil)
	contextHandler := logging.NewContextHandler(jsonHandler)
	slog.SetDefault(slog.New(contextHandler))
}

func initDatabase() *store.DBStore {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	}
	dbStore, err := store.NewDBStore(context.Background(), dbURL)
	if err != nil {
		// fatal will terminate the execution!!
		slog.Error("db init failed", "error", err)
		os.Exit(1)
	}
	return dbStore
}

func initRedis() *store.RedisStore {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "localhost:2907"
	}
	redisStore, err := store.NewRedisStore(context.Background(), redisURL)
	if err != nil {
		slog.Error("redis init failed", "error", err)
		os.Exit(1)
	}
	return redisStore
}

func registerAPIRoute(apiHandler *handler.APIHandler) {
	http.HandleFunc("/messages", apiHandler.HandleMessages)
	http.HandleFunc("/healthcheck", apiHandler.CheckServerHealth)
}

func registerWSRoute(wsHandler *handler.WebsocketHandler) {
	http.HandleFunc("/ws", wsHandler.HandleWebsocket)
}

func startServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	origins, err := middleware.ParseOrigins(os.Getenv("CORS_ORIGINS"))
	if err != nil {
		slog.Error("cors setting not valid", "error", err)
		os.Exit(1)
	}

	// 建立SERVER
	server := http.Server{
		Addr: ":" + port,
		Handler: middleware.Chain(
			http.DefaultServeMux, //  Go 內建的全域 router
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
