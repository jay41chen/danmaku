package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// 需要真的 Postgres：沒設 TEST_DATABASE_URL 就 skip（見 newTestDBStore）
func TestCreateUser(t *testing.T) {
	t.Run("new username returns id", func(t *testing.T) {
		s := newTestDBStore(t)

		id, err := s.CreateUser(t.Context(), "jay", "fake-hash")

		if err != nil {
			t.Fatalf("CreateUser(jay) error = %v, want nil", err)
		}
		// 不能檢查 id == 1：id 來自獨立的計數器（sequence），TRUNCATE 只清資料、不會把計數器歸零，
		// 失敗的 INSERT 也會用掉號碼，所以每次跑拿到的 id 都不一樣。這裡只確認有拿到 DB 給的 id。
		if id <= 0 {
			t.Errorf("CreateUser(jay) id = %d, want > 0", id)
		}
	})

	// 重複註冊的兩種寫法：Given 都是先建好一筆 user，差在第二次的 username
	duplicates := []struct {
		name     string
		existing string
		username string
	}{
		{"exactly same username", "jay", "jay"},
		{"same username in different case", "Jay", "jay"},
	}
	for _, tc := range duplicates {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestDBStore(t)
			if _, err := s.CreateUser(t.Context(), tc.existing, "fake-hash"); err != nil {
				t.Fatalf("setup CreateUser(%q) error = %v", tc.existing, err)
			}

			_, err := s.CreateUser(t.Context(), tc.username, "fake-hash")

			if !errors.Is(err, ErrUsernameTaken) {
				t.Errorf("CreateUser(%q) after %q error = %v, want ErrUsernameTaken", tc.username, tc.existing, err)
			}
		})
	}
}

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
	if _, err := s.dbPool.Exec(ctx, "TRUNCATE users"); err != nil {
		t.Fatalf("truncate users: %v", err)
	}
	return s
}
