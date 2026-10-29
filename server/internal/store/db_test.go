package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
)

// migration 000010 建的 legacy room（public_id = LegacyRoomPublicID）固定是 id 1，newTestDBStore 清資料時也會保留它
const legacyRoomID int64 = 1

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

func TestGetUserByID(t *testing.T) {
	t.Run("returns the stored row", func(t *testing.T) {
		s := newTestDBStore(t)
		// 先塞另一個 user：避免 WHERE 寫錯剛好拿到唯一一筆
		if _, err := s.CreateUser(t.Context(), "alice", "alice-hash"); err != nil {
			t.Fatalf("setup CreateUser(alice) error = %v", err)
		}
		id, err := s.CreateUser(t.Context(), "Jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser(Jay) error = %v", err)
		}

		u, err := s.GetUserByID(t.Context(), id)

		if err != nil {
			t.Fatalf("GetUserByID(%d) error = %v, want nil", id, err)
		}
		if u.ID != id {
			t.Errorf("GetUserByID(%d) ID = %d, want %d", id, u.ID, id)
		}
		if u.Username != "Jay" {
			t.Errorf("GetUserByID(%d) Username = %q, want %q", id, u.Username, "Jay")
		}
	})

	t.Run("unknown id returns ErrUserNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.GetUserByID(t.Context(), 999999)

		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUserByID(999999) error = %v, want ErrUserNotFound", err)
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
			// 用 raw SQL 存（不經 CreateMessage）：這個 test 只驗查詢，不該被 CreateMessage 的 SQL 影響
			// 照順序存：id 由 DB 遞增，所以先存的就是比較舊的
			for _, content := range tc.saved {
				if _, err := s.dbPool.Exec(t.Context(), "INSERT INTO messages(room_id, content) VALUES ($1, $2)", legacyRoomID, content); err != nil {
					t.Fatalf("setup insert %q error = %v", content, err)
				}
			}

			msgs, err := s.QueryMessage(t.Context(), legacyRoomID, tc.limit)

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

func TestCreateMessage(t *testing.T) {
	t.Run("returns full message with username", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "Jay")

		msg, err := s.CreateMessage(t.Context(), LegacyRoomPublicID, userID, "哈哈")

		if err != nil {
			t.Fatalf("CreateMessage error = %v, want nil", err)
		}
		if msg.ID <= 0 {
			t.Errorf("ID = %d, want > 0", msg.ID)
		}
		if msg.UserID == nil || *msg.UserID != userID {
			t.Errorf("UserID = %v, want %d", msg.UserID, userID)
		}
		// 回傳 users 表存的大小寫，不是別處傳進來的
		if msg.Username == nil || *msg.Username != "Jay" {
			t.Errorf("Username = %v, want Jay", msg.Username)
		}
		if msg.Content != "哈哈" {
			t.Errorf("Content = %q, want %q", msg.Content, "哈哈")
		}
		if msg.CreatedAt.IsZero() {
			t.Error("CreatedAt is zero, want DB timestamp")
		}
	})

	// user 已刪除但 access token 還有效：FK 違反要變成 sentinel，而且不能留下任何列
	t.Run("nonexistent user returns ErrMessageUserNotFound and inserts nothing", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.CreateMessage(t.Context(), LegacyRoomPublicID, 999999, "hi")

		if !errors.Is(err, ErrMessageUserNotFound) {
			t.Fatalf("CreateMessage error = %v, want ErrMessageUserNotFound", err)
		}
		if n := countMessages(t, s); n != 0 {
			t.Errorf("messages count = %d, want 0", n)
		}
	})
}

func countMessages(t *testing.T, s *DBStore) int {
	t.Helper()
	var count int
	if err := s.dbPool.QueryRow(t.Context(), "SELECT count(*) FROM messages").Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return count
}

func TestCreateMessageRooms(t *testing.T) {
	t.Run("message lands in the given room", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "r")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}

		msg, err := s.CreateMessage(t.Context(), room.PublicID, userID, "hi")

		if err != nil {
			t.Fatalf("CreateMessage error = %v, want nil", err)
		}
		var roomID int64
		if err := s.dbPool.QueryRow(t.Context(), "SELECT room_id FROM messages WHERE id = $1", msg.ID).Scan(&roomID); err != nil {
			t.Fatalf("query room_id: %v", err)
		}
		if roomID != room.ID {
			t.Errorf("room_id = %d, want %d", roomID, room.ID)
		}
	})

	t.Run("nonexistent room returns ErrRoomNotFound and inserts nothing", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")

		_, err := s.CreateMessage(t.Context(), "nope0000000", userID, "hi")

		if !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want ErrRoomNotFound", err)
		}
		if n := countMessages(t, s); n != 0 {
			t.Errorf("messages count = %d, want 0", n)
		}
	})

	// room 跟 user 都不存在：插入 0 列、FK 不會被檢查 → 房間優先（handler 回 404 不是 401）
	t.Run("nonexistent room and user returns ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.CreateMessage(t.Context(), "nope0000000", 999999, "hi")

		if !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want ErrRoomNotFound", err)
		}
	})

	t.Run("soft-deleted room returns ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "r")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}
		if _, err := s.dbPool.Exec(t.Context(), "UPDATE rooms SET deleted_at = now() WHERE id = $1", room.ID); err != nil {
			t.Fatalf("soft delete room: %v", err)
		}

		_, err = s.CreateMessage(t.Context(), room.PublicID, userID, "hi")

		if !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want ErrRoomNotFound", err)
		}
		if n := countMessages(t, s); n != 0 {
			t.Errorf("messages count = %d, want 0", n)
		}
	})

	t.Run("nonexistent user in existing room still returns ErrMessageUserNotFound", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "r")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}

		_, err = s.CreateMessage(t.Context(), room.PublicID, 999999, "hi")

		if !errors.Is(err, ErrMessageUserNotFound) {
			t.Fatalf("CreateMessage error = %v, want ErrMessageUserNotFound", err)
		}
	})
}

func TestQueryMessageRooms(t *testing.T) {
	t.Run("only returns messages of the given room", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		roomA, err := s.CreateRoom(t.Context(), userID, "a")
		if err != nil {
			t.Fatalf("setup CreateRoom a: %v", err)
		}
		roomB, err := s.CreateRoom(t.Context(), userID, "b")
		if err != nil {
			t.Fatalf("setup CreateRoom b: %v", err)
		}
		for _, c := range []struct {
			room    string
			content string
		}{{roomA.PublicID, "a1"}, {roomB.PublicID, "b1"}, {roomA.PublicID, "a2"}} {
			if _, err := s.CreateMessage(t.Context(), c.room, userID, c.content); err != nil {
				t.Fatalf("setup CreateMessage %q: %v", c.content, err)
			}
		}

		msgs, err := s.QueryMessage(t.Context(), roomA.ID, 50)

		if err != nil {
			t.Fatalf("QueryMessage error = %v", err)
		}
		got := []string{}
		for _, m := range msgs {
			got = append(got, m.Content)
		}
		if !slices.Equal(got, []string{"a2", "a1"}) {
			t.Errorf("contents = %q, want [a2 a1]", got)
		}
	})

	t.Run("room without messages returns empty non-nil slice", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "empty")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}

		msgs, err := s.QueryMessage(t.Context(), room.ID, 50)

		if err != nil {
			t.Fatalf("QueryMessage error = %v", err)
		}
		if msgs == nil || len(msgs) != 0 {
			t.Errorf("msgs = %#v, want empty non-nil slice", msgs)
		}
	})
}

func TestQueryMessageUserColumns(t *testing.T) {
	// LEFT JOIN 的重點：沒有發送者的舊資料不能被丟掉，Username / UserID 要是 nil
	t.Run("rows without user come back with nil user fields", func(t *testing.T) {
		s := newTestDBStore(t)
		if _, err := s.dbPool.Exec(t.Context(), "INSERT INTO messages(room_id, content) VALUES ($1, 'old')", legacyRoomID); err != nil {
			t.Fatalf("setup insert: %v", err)
		}

		msgs, err := s.QueryMessage(t.Context(), legacyRoomID, 50)

		if err != nil {
			t.Fatalf("QueryMessage error = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if msgs[0].UserID != nil || msgs[0].Username != nil {
			t.Errorf("UserID = %v, Username = %v, want both nil", msgs[0].UserID, msgs[0].Username)
		}
	})

	// 刪 user 後訊息要留著（ON DELETE SET NULL），並且仍查得到
	t.Run("deleting user keeps message with NULL user_id", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		if _, err := s.dbPool.Exec(t.Context(),
			"INSERT INTO messages(room_id, content, user_id) VALUES ($1, 'bye', $2)", legacyRoomID, userID); err != nil {
			t.Fatalf("setup insert: %v", err)
		}
		if _, err := s.dbPool.Exec(t.Context(), "DELETE FROM users WHERE id = $1", userID); err != nil {
			t.Fatalf("delete user: %v", err)
		}

		msgs, err := s.QueryMessage(t.Context(), legacyRoomID, 50)

		if err != nil {
			t.Fatalf("QueryMessage error = %v", err)
		}
		if len(msgs) != 1 || msgs[0].Content != "bye" {
			t.Fatalf("msgs = %+v, want the one 'bye' message kept", msgs)
		}
		if msgs[0].UserID != nil || msgs[0].Username != nil {
			t.Errorf("UserID = %v, Username = %v, want both nil", msgs[0].UserID, msgs[0].Username)
		}
	})

	t.Run("rows with user carry username", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "Jay")
		if _, err := s.dbPool.Exec(t.Context(),
			"INSERT INTO messages(room_id, content, user_id) VALUES ($1, 'hi', $2)", legacyRoomID, userID); err != nil {
			t.Fatalf("setup insert: %v", err)
		}

		msgs, err := s.QueryMessage(t.Context(), legacyRoomID, 50)

		if err != nil {
			t.Fatalf("QueryMessage error = %v", err)
		}
		if len(msgs) != 1 || msgs[0].Username == nil || *msgs[0].Username != "Jay" ||
			msgs[0].UserID == nil || *msgs[0].UserID != userID {
			t.Errorf("msgs = %+v, want user %d / Jay", msgs, userID)
		}
	})
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
