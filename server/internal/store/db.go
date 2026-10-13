package store

import (
	"context"
	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5/pgxpool"
)


type DBStoreInterface interface {
	SaveMessage(ctx context.Context, context string) error
    QueryMessage(ctx context.Context, limit int) ([]model.Message, error)
    CheckHealth(ctx context.Context) error
}

type DBStore struct { 
	dbPool *pgxpool.Pool
}

// constuctor
func NewDBStore(ctx context.Context, urlString string) (*DBStore, error) {
    pool, err := pgxpool.New(ctx, urlString)
    if err != nil {
        return nil, err
    }
    return &DBStore{dbPool: pool}, nil
}

func (this *DBStore) CheckHealth(ctx context.Context) error {
	return this.dbPool.Ping(ctx)
}

func (this *DBStore) SaveMessage(ctx context.Context, content string) error {
	_, err := this.dbPool.Exec(ctx,
		`
			INSERT 
			INTO messages (content) 
			VALUES ($1)
		`, content)
	return err
}

func (this *DBStore) QueryMessage(ctx context.Context, limit int) ([]model.Message, error) {
	rows, err := this.dbPool.Query(ctx, 
		`
			SELECT content, created_at
			FROM (
				SELECT id, content, created_at
				FROM messages
				ORDER BY id DESC
				LIMIT $1
			) sub
			 ORDER BY id ASC
		`, limit)	
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []model.Message{}
	for rows.Next() {
		var msg model.Message
		// 這行是幹嘛?為啥會有m?
		if err := rows.Scan(&msg.Content, &msg.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}