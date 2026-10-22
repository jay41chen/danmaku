package store

import (
	"context"
	"errors"
	"os"
	"slices"
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

func TestGetUser(t *testing.T) {
	// 存大寫、用小寫查：一次驗「比對不分大小寫」和「回傳保留原本大小寫」
	t.Run("lookup is case-insensitive and returns stored row", func(t *testing.T) {
		s := newTestDBStore(t)
		// 先塞另一個 user：表裡只有 Jay 的話，WHERE 寫錯（例如永遠回第一筆）也會剛好拿到 Jay
		if _, err := s.CreateUser(t.Context(), "alice", "alice-hash"); err != nil {
			t.Fatalf("setup CreateUser(alice) error = %v", err)
		}
		id, err := s.CreateUser(t.Context(), "Jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser(Jay) error = %v", err)
		}

		u, err := s.GetUser(t.Context(), "jay")

		if err != nil {
			t.Fatalf("GetUser(jay) error = %v, want nil", err)
		}
		if u.ID != id {
			t.Errorf("GetUser(jay) ID = %d, want %d", u.ID, id)
		}
		if u.Username != "Jay" {
			t.Errorf("GetUser(jay) Username = %q, want %q", u.Username, "Jay")
		}
		if u.PasswordHash != "fake-hash" {
			t.Errorf("GetUser(jay) PasswordHash = %q, want %q", u.PasswordHash, "fake-hash")
		}
	})

	t.Run("unknown username returns ErrUserNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.GetUser(t.Context(), "nobody")

		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUser(nobody) error = %v, want ErrUserNotFound", err)
		}
	})
}

func TestQueryMessage(t *testing.T) {
	// 三個 case 形狀一樣：存哪些訊息、limit 多少、預期拿回哪些內容（新→舊）
	cases := []struct {
		name  string
		saved []string
		limit int
		want  []string
	}{
		{name: "empty table", limit: 50, want: []string{}},
		{name: "fewer than limit", saved: []string{"a", "b", "c"}, limit: 50, want: []string{"c", "b", "a"}},
		// 最新的 3 則，新→舊
		{name: "more than limit", saved: []string{"a", "b", "c", "d", "e"}, limit: 3, want: []string{"e", "d", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestDBStore(t)
			// 照順序存：id 由 DB 遞增，所以先存的就是比較舊的
			for _, content := range tc.saved {
				if err := s.SaveMessage(t.Context(), content); err != nil {
					t.Fatalf("setup SaveMessage(%q) error = %v", content, err)
				}
			}

			msgs, err := s.QueryMessage(t.Context(), tc.limit)

			if err != nil {
				t.Fatalf("QueryMessage(%d) error = %v, want nil", tc.limit, err)
			}
			// nil slice 經 json.Encode 會變 null，client 預期的是 []
			// slices.Equal 不分 nil 跟空 slice，所以要另外檢查
			if msgs == nil {
				t.Errorf("QueryMessage(%d) = nil, want non-nil slice", tc.limit)
			}
			got := make([]string, 0, len(msgs))
			for _, m := range msgs {
				got = append(got, m.Content)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("QueryMessage(%d) contents = %q, want %q", tc.limit, got, tc.want)
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
	// refresh_tokens 有 foreign key 指向 users：只 TRUNCATE users 會被擋，要一起列出來
	if _, err := s.dbPool.Exec(ctx, "TRUNCATE users, messages, refresh_tokens"); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return s
}
