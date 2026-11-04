package store

import (
	"context"
	"os"
	"testing"
)

// migration 000010 建的 legacy room（public_id = LegacyRoomPublicID）固定是 id 1，newTestDBStore 清資料時也會保留它
const legacyRoomID int64 = 1

func newTestDBStore(t *testing.T) *DBStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := RunMigration(url); err != nil {
		t.Fatalf("RunMigration: %v", err)
	}

	ctx := context.Background()
	s, err := NewDBStore(ctx, url)
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	t.Cleanup(s.dbPool.Close)

	// 每個測試從空表開始，彼此不互相影響
	// 但 migration 000010 種下的 admin user 和 legacy 房間（id 1）要留著：
	//   messages.room_id 的 DEFAULT 1 指向它；TRUNCATE 不會重設 identity，砍掉就補不回 id 1
	// 所以不能 TRUNCATE rooms / users（rooms 有 FK 指向 users、messages 指向 rooms），改成依 FK 順序刪：
	//   messages、refresh_tokens 先清空 → 刪 legacy 以外的房間 → 刪 admin 以外的 user
	if _, err := s.dbPool.Exec(ctx, `
		TRUNCATE messages, refresh_tokens;
		DELETE FROM rooms WHERE public_id <> 'legacy00000';
		DELETE FROM users WHERE username <> 'admin';
	`); err != nil {
		t.Fatalf("reset tables: %v", err)
	}
	return s
}
