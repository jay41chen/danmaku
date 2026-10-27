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
	// user 已被刪除，但他的 access token 還沒過期（stateless，撤不掉）
	ErrMessageUserNotFound = errors.New("message user not found")
)

type DBStoreInterface interface {
	CreateMessage(ctx context.Context, userID int64, content string) (model.Message, error)
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

// 存訊息並回傳完整的 Message（含 id、username、created_at），handler 直接拿去廣播 / 回給 client
func (this *DBStore) CreateMessage(ctx context.Context, userID int64, content string) (model.Message, error) {
	var msg model.Message
	// 一個 statement 同時 INSERT 和 JOIN users 取 username，不用兩次來回
	err := this.dbPool.QueryRow(ctx,
		// TODO(user): CTE — WITH m AS (INSERT ... RETURNING ...) SELECT ... JOIN users，欄位順序要對上 Scan：m.id, m.user_id, u.username, m.content, m.created_at
		`
			WITH m AS (
				INSERT INTO messages(user_id, content)
				VALUES ($1, $2)
				RETURNING id, user_id, content, created_at
			)
			SELECT m.id, m.user_id, u.username, m.content, m.created_at
			FROM m
			JOIN users u ON m.user_id = u.id
		`, userID, content,
	).Scan(&msg.ID, &msg.UserID, &msg.Username, &msg.Content, &msg.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		// 23503 = foreign_key_violation：user_id 指向的 user 不存在（已被刪除）
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return model.Message{}, ErrMessageUserNotFound
		}
		return model.Message{}, err
	}
	return msg, nil
}

func (this *DBStore) QueryMessage(ctx context.Context, limit int) ([]model.Message, error) {
	// LEFT JOIN：舊資料和發送者已被刪除的訊息 user_id 是 NULL，INNER JOIN 會把這些列整個丟掉
	// 所以 Message.Username / UserID 是指標：NULL 掃不進 string / int64，只能掃進 *string / *int64
	rows, err := this.dbPool.Query(ctx,
		`
			SELECT m.id, m.user_id, u.username, m.content, m.created_at
			FROM messages m
			LEFT JOIN users u ON u.id = m.user_id
			ORDER BY m.id DESC
			LIMIT $1
		`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []model.Message{}
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(&msg.ID, &msg.UserID, &msg.Username, &msg.Content, &msg.CreatedAt); err != nil {
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
