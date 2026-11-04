package store

import (
	"context"
	"errors"

	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrUserNotFound  = errors.New("user not found")
)

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

// 用 id 查 user：WS auth 拿到 token 裡的 user id 後，確認這個 user 還在、並取得 username
func (this *DBStore) GetUserByID(ctx context.Context, id int64) (model.User, error) {
	var user model.User
	err := this.dbPool.QueryRow(ctx,
		`
			SELECT id, username, password_hash, created_at
			FROM users
			WHERE id = $1
		`, id).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return user, ErrUserNotFound
		}
		return user, err
	}
	return user, nil
}

// 一次做三件事：
//  1. 確認 user 還在 → 查不到回 ErrUserNotFound
//  2. 鎖住這個 user 的那一列直到 transaction 結束：同一個 user 的請求在這裡排隊
//     （CreateRoom 的房間數上限、RotateRefreshToken 的撤銷判斷）；不同 user 不會互相卡住
//  3. 拿 username
//
// 參數是 pgx.Tx 不是 pool：FOR UPDATE 離開 transaction 就沒效（單句自動 commit、鎖馬上放開），
// 讓 compiler 擋掉傳 pool（鎖要跟後面的動作在同一個 transaction）
func lockUser(ctx context.Context, tx pgx.Tx, userID int64) (string, error) {
	var username string
	err := tx.QueryRow(ctx,
		`
			SELECT username
			FROM users
			WHERE id = $1
			FOR UPDATE
		`, userID).Scan(&username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrUserNotFound
		}
		return "", err
	}
	return username, nil
}
