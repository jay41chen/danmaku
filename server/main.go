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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
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

	// 建立SERVER
	server := http.Server{
		Addr:    ":" + port,
		Handler: middleware.RecoverPanic(prom.HttpMetrics(http.DefaultServeMux)),
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
