package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var dbPool *pgxpool.Pool

type Message struct {
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func InitDB(connStr string) error {
	p, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		return err
	}
	dbPool = p
	return nil
}

func CheckDBHealth(ctx context.Context)error{
  return dbPool.Ping(ctx)
}

func SaveMessage(content string) error {
	_, err := dbPool.Exec(context.Background(),
		"INSERT INTO messages (content) VALUES ($1)", content)
	return err
}

func GetMessages(limit int) ([]Message, error) {
	rows, err := dbPool.Query(context.Background(),
		"SELECT content, created_at FROM (SELECT id, content, created_at FROM messages ORDER BY id DESC LIMIT $1) sub ORDER BY id ASC", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}
