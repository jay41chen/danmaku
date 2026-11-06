package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/randx"
)

// 測試資料的建立與清理：直接寫 DB（不走 bcrypt 與 /register），全部帶這次 run 專屬的前綴，
// 清理只刪帶這個前綴的資料——所以不會動到別人的資料，部分建立失敗也清得乾淨。

const accessTokenTTL = 3 * time.Hour

// 這次 run 的前綴。隨機 4 bytes，結尾的 _ 讓 starts_with 不會誤中別的前綴
func newRunPrefix() (prefix, run string) {
	b := make([]byte, 4)
	rand.Read(b)
	run = hex.EncodeToString(b)
	return "lt" + run + "_", run
}

type seeded struct {
	prefix  string
	ownerID int64
	userIDs []int64
	rooms   []string // public id
	tokens  []string // 絕不印出
}

// 建立 owner、users 個使用者與 rooms 個房間。password_hash 是 '!'（跟 migration 的 admin 一樣，永遠登不進去）
// 呼叫端要在呼叫前就 defer cleanup：這裡中途失敗時已建的部分也靠前綴清掉
func seedData(ctx context.Context, pool *pgxpool.Pool, prefix string, users, rooms int, issuer *auth.TokenIssuer) (*seeded, error) {
	s := &seeded{prefix: prefix}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users(username, password_hash) VALUES ($1, '!') RETURNING id`, prefix+"owner").Scan(&s.ownerID); err != nil {
		return nil, fmt.Errorf("seed owner: %w", err)
	}
	rows, err := pool.Query(ctx,
		`INSERT INTO users(username, password_hash)
		 SELECT $1 || 'u' || g, '!' FROM generate_series(1, $2::int) g
		 RETURNING id`, prefix, users)
	if err != nil {
		return nil, fmt.Errorf("seed users: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("seed users: %w", err)
		}
		s.userIDs = append(s.userIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seed users: %w", err)
	}

	batch := &pgx.Batch{}
	for i := range rooms {
		publicID := randx.URLText(8)
		s.rooms = append(s.rooms, publicID)
		batch.Queue(`INSERT INTO rooms(public_id, owner_id, room_name) VALUES ($1, $2, $3)`,
			publicID, s.ownerID, prefix+"room"+strconv.Itoa(i))
	}
	br := pool.SendBatch(ctx, batch)
	for range rooms {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return nil, fmt.Errorf("seed rooms: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("seed rooms: %w", err)
	}

	for _, id := range s.userIDs {
		token, _, err := issuer.IssueAccessToken(id)
		if err != nil {
			return nil, errors.New("mint access token failed")
		}
		s.tokens = append(s.tokens, token)
	}
	return s, nil
}

type cleanupResult struct {
	Messages int64  `json:"messages_deleted"`
	Rooms    int64  `json:"rooms_deleted"`
	Users    int64  `json:"users_deleted"`
	RedisKey int64  `json:"redis_rate_limit_keys_deleted"`
	Leftover int64  `json:"leftover_rows_after_cleanup"`
	Error    string `json:"error,omitempty"`
}

// 清掉這次 run 的資料：訊息 → 房間角色 → 房間 → 使用者（FK 順序），再刪這些 user 的限流 key。
// 一律用前綴比對，不依賴記憶體裡的 id 清單，所以 seed 做到一半失敗、或 Ctrl-C 都能用
// 用新的 context，不受已取消的 run context 影響
func cleanupData(pool *pgxpool.Pool, rdb *redis.Client, prefix string, userIDs []int64) cleanupResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var res cleanupResult
	fail := func(err error) cleanupResult {
		res.Error = err.Error()
		return res
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fail(fmt.Errorf("cleanup begin: %w", err))
	}
	defer tx.Rollback(ctx)
	myRooms := `SELECT r.id FROM rooms r JOIN users u ON u.id = r.owner_id WHERE starts_with(u.username, $1)`
	tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE room_id IN (`+myRooms+`)`, prefix)
	if err != nil {
		return fail(fmt.Errorf("cleanup messages: %w", err))
	}
	res.Messages = tag.RowsAffected()
	if _, err := tx.Exec(ctx, `DELETE FROM room_roles WHERE room_id IN (`+myRooms+`)`, prefix); err != nil {
		return fail(fmt.Errorf("cleanup room_roles: %w", err))
	}
	tag, err = tx.Exec(ctx,
		`DELETE FROM rooms WHERE owner_id IN (SELECT id FROM users WHERE starts_with(username, $1))`, prefix)
	if err != nil {
		return fail(fmt.Errorf("cleanup rooms: %w", err))
	}
	res.Rooms = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM users WHERE starts_with(username, $1)`, prefix)
	if err != nil {
		return fail(fmt.Errorf("cleanup users: %w", err))
	}
	res.Users = tag.RowsAffected()
	if err := tx.Commit(ctx); err != nil {
		return fail(fmt.Errorf("cleanup commit: %w", err))
	}

	// 驗證：不能再有帶這個前綴的 user，也不能有掛在它們名下的房間
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM users WHERE starts_with(username, $1))
		      + (SELECT count(*) FROM rooms WHERE starts_with(room_name, $1))`, prefix).Scan(&res.Leftover); err != nil {
		return fail(fmt.Errorf("cleanup verify: %w", err))
	}

	// 限流 key 本來就會在 10 秒內過期，順手刪掉；失敗不算清理失敗
	if rdb != nil {
		var keys []string
		for _, id := range userIDs {
			v := strconv.FormatInt(id, 10)
			keys = append(keys, "rl:message:burst:"+v, "rl:message:sustain:"+v)
		}
		for len(keys) > 0 {
			chunk := keys[:min(len(keys), 500)]
			keys = keys[len(chunk):]
			if n, err := rdb.Del(ctx, chunk...).Result(); err == nil {
				res.RedisKey += n
			}
		}
	}
	return res
}
