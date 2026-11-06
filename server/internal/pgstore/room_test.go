package pgstore

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5/pgconn"
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

	t.Run("not found returns model.ErrRoomNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.GetRoomByPublicID(t.Context(), "doesnotexist")

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Errorf("error = %v, want model.ErrRoomNotFound", err)
		}
	})

	t.Run("soft-deleted returns model.ErrRoomNotFound", func(t *testing.T) {
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

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Errorf("error = %v, want model.ErrRoomNotFound", err)
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
		// 產生器和檢查器要對得上：store 產生的 public_id 一定要通過 model.ValidPublicID（page token 等地方用它擋格式）
		if !model.ValidPublicID(room.PublicID) {
			t.Errorf("model.ValidPublicID(%q) = false, want true", room.PublicID)
		}
	})

	t.Run("4th room returns model.ErrRoomLimitReached", func(t *testing.T) {
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

		if !errors.Is(err, model.ErrRoomLimitReached) {
			t.Errorf("error = %v, want model.ErrRoomLimitReached", err)
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

	t.Run("nonexistent owner returns model.ErrRoomOwnerNotFound", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.CreateRoom(t.Context(), 999999999, "x")

		if !errors.Is(err, model.ErrRoomOwnerNotFound) {
			t.Errorf("error = %v, want model.ErrRoomOwnerNotFound", err)
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
			case errors.Is(err, model.ErrRoomLimitReached):
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

// migration 000010 / 000011 的結果：舊資料掛在 admin 的 legacy 房間（id 1）；000011 拿掉 DEFAULT 後，沒帶 room_id 的 INSERT 會被 NOT NULL 擋下
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

	t.Run("legacy public id resolves to room 1", func(t *testing.T) {
		s := newTestDBStore(t)
		userID, err := s.CreateUser(t.Context(), "jay", "fake-hash")
		if err != nil {
			t.Fatalf("setup CreateUser error = %v", err)
		}
		msg, _, err := s.CreateMessage(t.Context(), model.LegacyRoomPublicID, userID, "hi")
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

	// 鎖住 000011 的 contract：DEFAULT 被加回來的話，忘了帶 room_id 的 INSERT 會默默落到 legacy 房間，這個 test 會抓到
	t.Run("insert without room_id fails with not null violation", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.dbPool.Exec(t.Context(), `INSERT INTO messages(content) VALUES ('hi')`)

		var pgErr *pgconn.PgError
		// 23502 = not_null_violation
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "room_id" {
			t.Fatalf("error = %v, want not_null_violation on room_id", err)
		}
	})
}

// 所有測試房間都比 legacy 房間（migration 時間）新，所以 created_at DESC 時 legacy 一定排最後
var roomListBase = time.Date(2030, 1, 1, 0, 0, 0, 123456000, time.UTC)

func insertListRoom(t *testing.T, s *DBStore, publicID string, ownerID int64, createdAt time.Time, deletedAt *time.Time) {
	t.Helper()
	if _, err := s.dbPool.Exec(t.Context(),
		`INSERT INTO rooms(public_id, owner_id, room_name, created_at, deleted_at) VALUES ($1, $2, 'r', $3, $4)`,
		publicID, ownerID, createdAt, deletedAt); err != nil {
		t.Fatalf("setup insert room %s: %v", publicID, err)
	}
}

// 建 n 個房間 room0000000..，index 越小越新（created_at 每個差 1 秒），回傳依 DESC 排序的 public_id
func insertListRooms(t *testing.T, s *DBStore, ownerID int64, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("room%07d", i)
		insertListRoom(t, s, ids[i], ownerID, roomListBase.Add(-time.Duration(i)*time.Second), nil)
	}
	return ids
}

func roomIDs(rooms []model.Room) []string {
	ids := make([]string, len(rooms))
	for i, r := range rooms {
		ids[i] = r.PublicID
	}
	return ids
}

func TestGetRoomLists(t *testing.T) {
	// 5 個房間交錯屬於 a / b：index 偶數 a、奇數 b
	setupTwoOwners := func(t *testing.T) (s *DBStore, a, b int64, all []string) {
		s = newTestDBStore(t)
		a = createTestUser(t, s, "A")
		b = createTestUser(t, s, "B")
		for i := range 5 {
			id := fmt.Sprintf("room%07d", i)
			owner := a
			if i%2 == 1 {
				owner = b
			}
			insertListRoom(t, s, id, owner, roomListBase.Add(-time.Duration(i)*time.Second), nil)
			all = append(all, id)
		}
		return
	}

	t.Run("all owners first page", func(t *testing.T) {
		s, _, _, all := setupTwoOwners(t)

		rooms, cursor, err := s.GetRoomLists(t.Context(), nil, nil, 3)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		if got := roomIDs(rooms); !slices.Equal(got, all[:3]) {
			t.Errorf("ids = %v, want %v", got, all[:3])
		}
		if cursor == nil || cursor.PublicID != all[2] || !cursor.CreatedAt.Equal(rooms[2].CreatedAt) {
			t.Errorf("cursor = %+v, want last returned room %s", cursor, all[2])
		}
	})

	t.Run("all owners with cursor", func(t *testing.T) {
		s, _, _, all := setupTwoOwners(t)
		after := &model.RoomCursor{CreatedAt: roomListBase.Add(-2 * time.Second), PublicID: all[2]}

		rooms, cursor, err := s.GetRoomLists(t.Context(), nil, after, 10)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		// 剩下 room3、room4，最後是 legacy 房間
		want := []string{all[3], all[4], model.LegacyRoomPublicID}
		if got := roomIDs(rooms); !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
		if cursor != nil {
			t.Errorf("cursor = %+v, want nil", cursor)
		}
	})

	t.Run("owner only first page", func(t *testing.T) {
		s, a, _, all := setupTwoOwners(t)

		rooms, cursor, err := s.GetRoomLists(t.Context(), &a, nil, 2)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		want := []string{all[0], all[2]}
		if got := roomIDs(rooms); !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
		for _, r := range rooms {
			if r.OwnerID != a {
				t.Errorf("room %s owner = %d, want %d", r.PublicID, r.OwnerID, a)
			}
		}
		if cursor == nil || cursor.PublicID != all[2] {
			t.Errorf("cursor = %+v, want public_id %s", cursor, all[2])
		}
	})

	t.Run("owner only with cursor", func(t *testing.T) {
		s, a, _, all := setupTwoOwners(t)
		after := &model.RoomCursor{CreatedAt: roomListBase.Add(-2 * time.Second), PublicID: all[2]}

		rooms, cursor, err := s.GetRoomLists(t.Context(), &a, after, 10)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		want := []string{all[4]}
		if got := roomIDs(rooms); !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
		if cursor != nil {
			t.Errorf("cursor = %+v, want nil", cursor)
		}
	})

	t.Run("same created_at across page boundary", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		for _, id := range []string{"same000000a", "same000000b", "same000000c"} {
			insertListRoom(t, s, id, owner, roomListBase, nil)
		}

		page1, cursor, err := s.GetRoomLists(t.Context(), &owner, nil, 2)
		if err != nil {
			t.Fatalf("page 1 error = %v", err)
		}
		if want := []string{"same000000c", "same000000b"}; !slices.Equal(roomIDs(page1), want) {
			t.Errorf("page 1 ids = %v, want %v", roomIDs(page1), want)
		}
		if cursor == nil {
			t.Fatal("page 1 cursor = nil, want non-nil")
		}

		page2, cursor2, err := s.GetRoomLists(t.Context(), &owner, cursor, 2)
		if err != nil {
			t.Fatalf("page 2 error = %v", err)
		}
		if want := []string{"same000000a"}; !slices.Equal(roomIDs(page2), want) {
			t.Errorf("page 2 ids = %v, want %v", roomIDs(page2), want)
		}
		if cursor2 != nil {
			t.Errorf("page 2 cursor = %+v, want nil", cursor2)
		}
	})

	t.Run("paging through everything", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		want := insertListRooms(t, s, owner, 7)

		var got []string
		var after *model.RoomCursor
		for range 10 { // 防止無窮迴圈
			rooms, cursor, err := s.GetRoomLists(t.Context(), &owner, after, 3)
			if err != nil {
				t.Fatalf("GetRoomLists error = %v", err)
			}
			got = append(got, roomIDs(rooms)...)
			if cursor == nil {
				break
			}
			after = cursor
		}

		if !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("soft deleted excluded", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		deleted := roomListBase.Add(time.Hour)
		insertListRoom(t, s, "room0000001", owner, roomListBase, nil)
		insertListRoom(t, s, "room0000002", owner, roomListBase.Add(-time.Second), &deleted)
		insertListRoom(t, s, "room0000003", owner, roomListBase.Add(-2*time.Second), nil)

		mine, _, err := s.GetRoomLists(t.Context(), &owner, nil, 10)
		if err != nil {
			t.Fatalf("owner list error = %v", err)
		}
		if want := []string{"room0000001", "room0000003"}; !slices.Equal(roomIDs(mine), want) {
			t.Errorf("owner ids = %v, want %v", roomIDs(mine), want)
		}

		all, _, err := s.GetRoomLists(t.Context(), nil, nil, 10)
		if err != nil {
			t.Fatalf("all list error = %v", err)
		}
		if want := []string{"room0000001", "room0000003", model.LegacyRoomPublicID}; !slices.Equal(roomIDs(all), want) {
			t.Errorf("all ids = %v, want %v", roomIDs(all), want)
		}
	})

	// cursor 之後的 SQL 也要過濾 soft delete
	t.Run("soft deleted excluded after cursor", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		deleted := roomListBase.Add(time.Hour)
		insertListRoom(t, s, "room0000001", owner, roomListBase, nil)
		insertListRoom(t, s, "room0000002", owner, roomListBase.Add(-time.Second), &deleted)
		insertListRoom(t, s, "room0000003", owner, roomListBase.Add(-2*time.Second), nil)
		after := &model.RoomCursor{CreatedAt: roomListBase, PublicID: "room0000001"}

		mine, _, err := s.GetRoomLists(t.Context(), &owner, after, 10)
		if err != nil {
			t.Fatalf("owner list error = %v", err)
		}
		if want := []string{"room0000003"}; !slices.Equal(roomIDs(mine), want) {
			t.Errorf("owner ids = %v, want %v", roomIDs(mine), want)
		}

		all, _, err := s.GetRoomLists(t.Context(), nil, after, 10)
		if err != nil {
			t.Fatalf("all list error = %v", err)
		}
		if want := []string{"room0000003", model.LegacyRoomPublicID}; !slices.Equal(roomIDs(all), want) {
			t.Errorf("all ids = %v, want %v", roomIDs(all), want)
		}
	})

	// created_at 全部相同時，翻頁順序完全靠 public_id DESC 決定（4 條 SQL 都要覆蓋）
	t.Run("tiebreak by public_id across pages", func(t *testing.T) {
		tests := []struct {
			name      string
			allOwners bool
			want      []string
		}{
			{"owner only", false, []string{"ddddddddddd", "ccccccccccc", "bbbbbbbbbbb", "aaaaaaaaaaa"}},
			{"all owners", true, []string{"ddddddddddd", "ccccccccccc", "bbbbbbbbbbb", "aaaaaaaaaaa", model.LegacyRoomPublicID}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := newTestDBStore(t)
				owner := createTestUser(t, s, "A")
				// 故意不照順序插入
				for _, id := range []string{"bbbbbbbbbbb", "ddddddddddd", "aaaaaaaaaaa", "ccccccccccc"} {
					insertListRoom(t, s, id, owner, roomListBase, nil)
				}
				var userID *int64
				if !tt.allOwners {
					userID = &owner
				}

				var got []string
				var after *model.RoomCursor
				for page := 0; page < 10; page++ { // 防止無窮迴圈
					rooms, cursor, err := s.GetRoomLists(t.Context(), userID, after, 2)
					if err != nil {
						t.Fatalf("page %d error = %v", page+1, err)
					}
					if page == 0 {
						if want := []string{"ddddddddddd", "ccccccccccc"}; !slices.Equal(roomIDs(rooms), want) {
							t.Errorf("page 1 ids = %v, want %v", roomIDs(rooms), want)
						}
					}
					got = append(got, roomIDs(rooms)...)
					if cursor == nil {
						break
					}
					after = cursor
				}

				if !slices.Equal(got, tt.want) {
					t.Errorf("ids = %v, want %v", got, tt.want)
				}
			})
		}
	})

	t.Run("exactly limit rows has no cursor", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		want := insertListRooms(t, s, owner, 3)

		rooms, cursor, err := s.GetRoomLists(t.Context(), &owner, nil, 3)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		if !slices.Equal(roomIDs(rooms), want) {
			t.Errorf("ids = %v, want %v", roomIDs(rooms), want)
		}
		if cursor != nil {
			t.Errorf("cursor = %+v, want nil", cursor)
		}
	})

	t.Run("limit plus one rows cursor is last returned row", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		ids := insertListRooms(t, s, owner, 4)

		rooms, cursor, err := s.GetRoomLists(t.Context(), &owner, nil, 3)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		if !slices.Equal(roomIDs(rooms), ids[:3]) {
			t.Errorf("ids = %v, want %v", roomIDs(rooms), ids[:3])
		}
		wantTime := roomListBase.Add(-2 * time.Second)
		if cursor == nil || cursor.PublicID != ids[2] || !cursor.CreatedAt.Equal(wantTime) {
			t.Errorf("cursor = %+v, want {%v %s}", cursor, wantTime, ids[2])
		}
	})

	t.Run("empty result", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")

		rooms, cursor, err := s.GetRoomLists(t.Context(), &owner, nil, 10)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		if rooms == nil || len(rooms) != 0 {
			t.Errorf("rooms = %#v, want non-nil empty slice", rooms)
		}
		if cursor != nil {
			t.Errorf("cursor = %+v, want nil", cursor)
		}
	})

	t.Run("invalid limit", func(t *testing.T) {
		s := newTestDBStore(t)
		for _, limit := range []int{0, -1, model.MaxRoomListLimit + 1} {
			rooms, cursor, err := s.GetRoomLists(t.Context(), nil, nil, limit)
			if !errors.Is(err, model.ErrInvalidLimit) {
				t.Errorf("limit %d: error = %v, want model.ErrInvalidLimit", limit, err)
			}
			if rooms != nil || cursor != nil {
				t.Errorf("limit %d: rooms = %v, cursor = %v, want nil", limit, rooms, cursor)
			}
		}

		if _, _, err := s.GetRoomLists(t.Context(), nil, nil, model.MaxRoomListLimit); err != nil {
			t.Errorf("limit %d: error = %v, want nil", model.MaxRoomListLimit, err)
		}
	})

	t.Run("microsecond precision", func(t *testing.T) {
		s := newTestDBStore(t)
		owner := createTestUser(t, s, "A")
		created := time.Date(2030, 6, 7, 8, 9, 10, 654321000, time.UTC)
		insertListRoom(t, s, "room0000001", owner, created, nil)
		insertListRoom(t, s, "room0000002", owner, created.Add(-time.Second), nil)

		_, cursor, err := s.GetRoomLists(t.Context(), &owner, nil, 1)

		if err != nil {
			t.Fatalf("GetRoomLists error = %v, want nil", err)
		}
		if cursor == nil || !cursor.CreatedAt.Equal(created) {
			t.Errorf("cursor = %+v, want CreatedAt %v", cursor, created)
		}
	})
}
