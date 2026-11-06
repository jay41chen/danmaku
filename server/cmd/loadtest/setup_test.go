package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jay41chen/danmaku/server/internal/auth"
)

// 需要真的 Postgres：沒設 TEST_DATABASE_URL 就 skip；設了也要先過工具自己的安全檢查（只准 danmaku_test）
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if _, err := checkDatabaseURL(url); err != nil {
		t.Fatalf("refusing to run against this database: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 清理只刪帶自己前綴的資料：另一個 run 的資料與不相干的 user 都要原封不動
func TestCleanupRemovesOnlyOwnPrefix(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	issuer, err := auth.NewTokenIssuer(strings.Repeat("k", auth.MinSecretLength), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	prefixA, _ := newRunPrefix()
	prefixB, _ := newRunPrefix()
	decoy := prefixB + "decoy-not-from-the-tool" // 不同前綴
	decoy = strings.Replace(decoy, "lt", "zz", 1)
	// 前綴的 _ 換成 X：如果清理誤用 LIKE（_ 是萬用字元），這個名稱會被誤刪
	decoyWildcard := strings.Replace(prefixA, "_", "X", 1) + "owner"
	t.Cleanup(func() {
		cleanupData(pool, nil, prefixA, nil)
		cleanupData(pool, nil, prefixB, nil)
		pool.Exec(ctx, `DELETE FROM users WHERE username = ANY($1)`, []string{decoy, decoyWildcard})
	})

	a, err := seedData(ctx, pool, prefixA, 3, 2, issuer)
	if err != nil {
		t.Fatal(err)
	}
	b, err := seedData(ctx, pool, prefixB, 3, 2, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(username, password_hash) VALUES ($1, '!')`, decoy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(username, password_hash) VALUES ($1, '!')`, decoyWildcard); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*seeded{a, b} {
		if len(s.tokens) != 3 || s.tokens[0] == "" {
			t.Fatalf("tokens = %d, want 3 minted", len(s.tokens))
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO messages(room_id, user_id, content) SELECT id, $2, 'lt-test' FROM rooms WHERE public_id = $1`,
			s.rooms[0], s.userIDs[0]); err != nil {
			t.Fatal(err)
		}
	}

	res := cleanupData(pool, nil, prefixA, a.userIDs)

	if res.Error != "" || res.Leftover != 0 {
		t.Fatalf("cleanup = %+v", res)
	}
	if res.Messages != 1 || res.Rooms != 2 || res.Users != 4 { // 3 users + owner
		t.Errorf("deleted messages/rooms/users = %d/%d/%d, want 1/2/4", res.Messages, res.Rooms, res.Users)
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE starts_with(username, $1)`, prefixA); n != 0 {
		t.Errorf("run A users left = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE starts_with(username, $1)`, prefixB); n != 4 {
		t.Errorf("run B users = %d, want 4 untouched", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM rooms WHERE starts_with(room_name, $1)`, prefixB); n != 2 {
		t.Errorf("run B rooms = %d, want 2 untouched", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM messages m JOIN rooms r ON r.id = m.room_id WHERE starts_with(r.room_name, $1)`, prefixB); n != 1 {
		t.Errorf("run B messages = %d, want 1 untouched", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE username = $1`, decoy); n != 1 {
		t.Errorf("decoy user count = %d, want 1 untouched", n)
	}

	if n := count(t, pool, `SELECT count(*) FROM users WHERE username = $1`, decoyWildcard); n != 1 {
		t.Errorf("wildcard decoy %q count = %d, want 1 untouched (the prefix's _ must not act as a wildcard)", decoyWildcard, n)
	}

	// 清兩次也沒問題（Ctrl-C 後又清一次的情況）
	if again := cleanupData(pool, nil, prefixA, nil); again.Error != "" || again.Users != 0 {
		t.Errorf("second cleanup = %+v", again)
	}
}

// seed 做到一半失敗（這裡用重複的房間 public id 造成）：已建的 user 也靠前綴清得掉
func TestCleanupAfterPartialSeed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	issuer, _ := auth.NewTokenIssuer(strings.Repeat("k", auth.MinSecretLength), time.Hour)
	prefix, _ := newRunPrefix()
	t.Cleanup(func() { cleanupData(pool, nil, prefix, nil) })

	// 先佔住 prefix 的 owner 名稱，讓 seedData 在第一步就失敗
	if _, err := pool.Exec(ctx, `INSERT INTO users(username, password_hash) VALUES ($1, '!')`, prefix+"owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := seedData(ctx, pool, prefix, 2, 1, issuer); err == nil {
		t.Fatal("seedData succeeded, want failure on the duplicate owner")
	}

	res := cleanupData(pool, nil, prefix, nil)
	if res.Error != "" || res.Users != 1 || res.Leftover != 0 {
		t.Errorf("cleanup after partial seed = %+v, want 1 user removed and nothing left", res)
	}
}
