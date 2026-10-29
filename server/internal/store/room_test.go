package store

import (
	"errors"
	"regexp"
	"sync"
	"testing"
)

// 需要真的 Postgres：沒設 TEST_DATABASE_URL 就 skip（見 newTestDBStore）

func TestGetRoomByPublicID(t *testing.T) {
	t.Run("found returns room with owner username", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "Jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		if _, err := s.dbPool.Exec(t.Context(),
			`INSERT INTO rooms(public_id, owner_id, room_name) VALUES ('room0000001', $1, '房間')`, ownerID); err != nil {
			t.Fatalf("setup insert room: %v", err)
		}

		room, err := s.GetRoomByPublicID(t.Context(), "room0000001")

		if err != nil {
			t.Fatalf("GetRoomByPublicID error = %v, want nil", err)
		}
		if room.PublicID != "room0000001" || room.Name != "房間" || room.OwnerUsername != "Jay" || room.OwnerID != ownerID {
			t.Errorf("room = %+v, want public_id room0000001 / name 房間 / owner Jay (%d)", room, ownerID)
		}
		if room.ID <= 0 || room.CreatedAt.IsZero() {
			t.Errorf("room = %+v, want ID > 0 and CreatedAt set", room)
		}
	})

	t.Run("not found returns ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.GetRoomByPublicID(t.Context(), "doesnotexist")

		if !errors.Is(err, ErrRoomNotFound) {
			t.Errorf("error = %v, want ErrRoomNotFound", err)
		}
	})

	t.Run("soft-deleted returns ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		if _, err := s.dbPool.Exec(t.Context(),
			`INSERT INTO rooms(public_id, owner_id, room_name, deleted_at) VALUES ('room0000002', $1, 'x', now())`, ownerID); err != nil {
			t.Fatalf("setup insert room: %v", err)
		}

		_, err = s.GetRoomByPublicID(t.Context(), "room0000002")

		if !errors.Is(err, ErrRoomNotFound) {
			t.Errorf("error = %v, want ErrRoomNotFound", err)
		}
	})
}

func TestCreateRoom(t *testing.T) {
	t.Run("success returns room with owner username", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "Jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}

		room, err := s.CreateRoom(t.Context(), ownerID, "房間")

		if err != nil {
			t.Fatalf("CreateRoom error = %v, want nil", err)
		}
		if room.ID <= 0 || room.OwnerID != ownerID || room.OwnerUsername != "Jay" || room.Name != "房間" || room.CreatedAt.IsZero() {
			t.Errorf("room = %+v, want owner Jay (%d) / name 房間", room, ownerID)
		}
		// 真的寫進 DB 了
		got, err := s.GetRoomByPublicID(t.Context(), room.PublicID)
		if err != nil || got.ID != room.ID {
			t.Errorf("GetRoomByPublicID = (%+v, %v), want room id %d", got, err, room.ID)
		}
	})

	t.Run("public id is 11 base64url chars", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}

		room, err := s.CreateRoom(t.Context(), ownerID, "x")

		if err != nil {
			t.Fatalf("CreateRoom error = %v, want nil", err)
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`).MatchString(room.PublicID) {
			t.Errorf("public id = %q, want 11 base64url chars", room.PublicID)
		}
	})

	t.Run("4th room returns ErrRoomLimitReached", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		for i := 0; i < maxRoomsPerUser; i++ {
			if _, err := s.CreateRoom(t.Context(), ownerID, "x"); err != nil {
				t.Fatalf("setup CreateRoom #%d error = %v", i+1, err)
			}
		}

		_, err = s.CreateRoom(t.Context(), ownerID, "x")

		if !errors.Is(err, ErrRoomLimitReached) {
			t.Errorf("error = %v, want ErrRoomLimitReached", err)
		}
	})

	t.Run("soft-deleted rooms do not count", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		for i := 0; i < maxRoomsPerUser; i++ {
			if _, err := s.CreateRoom(t.Context(), ownerID, "x"); err != nil {
				t.Fatalf("setup CreateRoom #%d error = %v", i+1, err)
			}
		}
		if _, err := s.dbPool.Exec(t.Context(),
			`UPDATE rooms SET deleted_at = now() WHERE id = (SELECT min(id) FROM rooms WHERE owner_id = $1)`, ownerID); err != nil {
			t.Fatalf("setup soft delete: %v", err)
		}

		_, err = s.CreateRoom(t.Context(), ownerID, "x")

		if err != nil {
			t.Errorf("CreateRoom after soft delete error = %v, want nil", err)
		}
	})

	t.Run("nonexistent owner returns ErrRoomOwnerNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.CreateRoom(t.Context(), 999999999, "x")

		if !errors.Is(err, ErrRoomOwnerNotFound) {
			t.Errorf("error = %v, want ErrRoomOwnerNotFound", err)
		}
	})

	// 同一個 owner 同時建 10 個：鎖住 owner 的 user 列讓它們排隊，額度檢查才不會被穿過去
	t.Run("concurrent creates respect the limit", func(t *testing.T) {
		s := newTestDBStore(t)
		ownerID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.CreateRoom(t.Context(), ownerID, "x")
			}()
		}
		wg.Wait()

		var ok, limited int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrRoomLimitReached):
				limited++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != maxRoomsPerUser || limited != n-maxRoomsPerUser {
			t.Errorf("succeeded = %d, limited = %d, want %d / %d", ok, limited, maxRoomsPerUser, n-maxRoomsPerUser)
		}
	})
}

// migration 000010 的結果：舊資料掛在 admin 的 legacy 房間；沒帶 room_id 的 INSERT 落在房間 1
func TestRoomsMigration(t *testing.T) {
	t.Run("legacy room exists and is owned by admin", func(t *testing.T) {
		s := newTestDBStore(t)

		var id int64
		var owner, name string
		err := s.dbPool.QueryRow(t.Context(),
			`SELECT r.id, u.username, r.room_name FROM rooms r JOIN users u ON u.id = r.owner_id WHERE r.public_id = 'legacy00000'`,
		).Scan(&id, &owner, &name)

		if err != nil {
			t.Fatalf("query legacy room: %v", err)
		}
		if id != 1 || owner != "admin" || name != "Legacy" {
			t.Errorf("legacy room = (%d, %q, %q), want (1, admin, Legacy)", id, owner, name)
		}
	})

	t.Run("message without room_id lands in legacy room", func(t *testing.T) {
		s := newTestDBStore(t)
		userID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		msg, err := s.CreateMessage(t.Context(), userID, "hi")
		if err != nil {
			t.Fatalf("setup CreateMessage error = %v", err)
		}

		var roomID int64
		err = s.dbPool.QueryRow(t.Context(), `SELECT room_id FROM messages WHERE id = $1`, msg.ID).Scan(&roomID)

		if err != nil {
			t.Fatalf("query room_id: %v", err)
		}
		if roomID != 1 {
			t.Errorf("room_id = %d, want 1", roomID)
		}
	})
}
