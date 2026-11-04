package store

import (
	"context"
	"log/slog"

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
	return &DBStore{dbPool: pool, logger: logger}, nil
}

func (this *DBStore) CheckHealth(ctx context.Context) error {
	return this.dbPool.Ping(ctx)
}
