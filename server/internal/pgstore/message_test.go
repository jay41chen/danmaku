package pgstore

import (
	"errors"
	"slices"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

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

		msg, _, err := s.CreateMessage(t.Context(), model.LegacyRoomPublicID, userID, "哈哈")

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
	t.Run("nonexistent user returns model.ErrMessageUserNotFound and inserts nothing", func(t *testing.T) {
		s := newTestDBStore(t)

		_, _, err := s.CreateMessage(t.Context(), model.LegacyRoomPublicID, 999999, "hi")

		if !errors.Is(err, model.ErrMessageUserNotFound) {
			t.Fatalf("CreateMessage error = %v, want model.ErrMessageUserNotFound", err)
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

		msg, _, err := s.CreateMessage(t.Context(), room.PublicID, userID, "hi")

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

	t.Run("nonexistent room returns model.ErrRoomNotFound and inserts nothing", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")

		_, _, err := s.CreateMessage(t.Context(), "nope0000000", userID, "hi")

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want model.ErrRoomNotFound", err)
		}
		if n := countMessages(t, s); n != 0 {
			t.Errorf("messages count = %d, want 0", n)
		}
	})

	// room 跟 user 都不存在：插入 0 列、FK 不會被檢查 → 房間優先（handler 回 404 不是 401）
	t.Run("nonexistent room and user returns model.ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, _, err := s.CreateMessage(t.Context(), "nope0000000", 999999, "hi")

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want model.ErrRoomNotFound", err)
		}
	})

	t.Run("soft-deleted room returns model.ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "r")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}
		if _, err := s.dbPool.Exec(t.Context(), "UPDATE rooms SET deleted_at = now() WHERE id = $1", room.ID); err != nil {
			t.Fatalf("soft delete room: %v", err)
		}

		_, _, err = s.CreateMessage(t.Context(), room.PublicID, userID, "hi")

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Fatalf("CreateMessage error = %v, want model.ErrRoomNotFound", err)
		}
		if n := countMessages(t, s); n != 0 {
			t.Errorf("messages count = %d, want 0", n)
		}
	})

	t.Run("nonexistent user in existing room still returns model.ErrMessageUserNotFound", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		room, err := s.CreateRoom(t.Context(), userID, "r")
		if err != nil {
			t.Fatalf("setup CreateRoom: %v", err)
		}

		_, _, err = s.CreateMessage(t.Context(), room.PublicID, 999999, "hi")

		if !errors.Is(err, model.ErrMessageUserNotFound) {
			t.Fatalf("CreateMessage error = %v, want model.ErrMessageUserNotFound", err)
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
			if _, _, err := s.CreateMessage(t.Context(), c.room, userID, c.content); err != nil {
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
