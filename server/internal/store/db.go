package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrUserNotFound  = errors.New("user not found")
)

type DBStoreInterface interface {
	SaveMessage(ctx context.Context, content string) error
	QueryMessage(ctx context.Context, limit int) ([]model.Message, error)
	CheckHealth(ctx context.Context) error

	CreateUser(ctx context.Context, username string, passwordHash string) (int64, error)
	GetUser(ctx context.Context, username string) (model.User, error)

	// 實作在 refresh_token.go
	CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error)
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
}

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
			FROM messages
			ORDER BY id DESC
			LIMIT $1
		`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []model.Message{}
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(&msg.Content, &msg.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}

	// rows.Next() 讀完跟讀到一半出錯（例如連線斷掉）都回 false，迴圈分不出來
	// pgx 把中途的錯誤存在 rows 裡，只能在迴圈結束後用 rows.Err() 問；放在迴圈前一定是 nil
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return msgs, nil
}

func (this *DBStore) CreateUser(ctx context.Context, username string, passwordHash string) (int64, error) {
	var id int64
	// 用 Exec 還是 Query的取捨：=> 是否需要 DB 回傳資料列。
	err := this.dbPool.QueryRow(ctx,
		`
			INSERT INTO users(username, password_hash)
			VALUES($1, $2)
			RETURNING id
		`, username, passwordHash,
	).Scan(&id)

	if err != nil {
		var pgErr *pgconn.PgError
		// ┌───────────────────────────┬───────────────────────────────┬─────────────────────────────┐
		// │           規則             │             例子              │      違反時的 SQLSTATE      │
		// ├───────────────────────────┼───────────────────────────────┼─────────────────────────────┤
		// │ NOT NULL                  │ username TEXT NOT NULL        │ 23502                       │
		// ├───────────────────────────┼───────────────────────────────┼─────────────────────────────┤
		// │ UNIQUE（含 unique index）  │ users_username_lower_key      │ 23505                       │
		// ├───────────────────────────┼───────────────────────────────┼─────────────────────────────┤
		// │ PRIMARY KEY               │ id ... PRIMARY KEY            │ 23505（PK 也是一種 unique） │
		// ├───────────────────────────┼───────────────────────────────┼─────────────────────────────┤
		// │ FOREIGN KEY               │ user_id REFERENCES users(id)  │ 23503                       │
		// ├───────────────────────────┼───────────────────────────────┼─────────────────────────────┤
		// │ CHECK                     │ CHECK (length(username) >= 3) │ 23514                       │
		// └───────────────────────────┴───────────────────────────────┴─────────────────────────────┘
		//
		// 所以要透過`code`和`constraint name`同時比對才能確定正確的錯誤
		if errors.As(err, &pgErr) && (pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_lower_key") {
			return 0, ErrUsernameTaken
		}
		return 0, err
	}
	return id, nil
}

func (this *DBStore) GetUser(ctx context.Context, username string) (model.User, error) {
	var user model.User
	err := this.dbPool.QueryRow(ctx,
		`
			SELECT id, username, password_hash, created_at
			FROM users
			WHERE lower(username) = lower($1)
		`, username).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return user, ErrUserNotFound
		}
		return user, err
	}
	return user, nil
}
