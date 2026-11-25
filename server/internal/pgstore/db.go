package pgstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DBStore struct {
	dbPool *pgxpool.Pool
	logger *slog.Logger
}

// constuctor
func NewDBStore(ctx context.Context, urlString string) (*DBStore, error) {
	logger := slog.Default().With("component", "db")
	pool, err := pgxpool.New(ctx, urlString)
	if err != nil {
		return nil, err
	}
	// pgxpool.New 不會真的連線，沒有這步的話 DB 連不上要到第一個 query 才發現；
	// 目前 RunMigration 先跑，會先擋下這種情況，但之後 migration 若拆成獨立的部署步驟就會漏掉（同 Redis 啟動時的 Ping）
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &DBStore{dbPool: pool, logger: logger}, nil
}

func (this *DBStore) CheckHealth(ctx context.Context) error {
	return this.dbPool.Ping(ctx)
}

// 壓測計時用（internal/loadtiming）：pool 當下的統計快照；沒有人呼叫時不產生任何額外工作
func (this *DBStore) PoolStat() *pgxpool.Stat {
	return this.dbPool.Stat()
}
