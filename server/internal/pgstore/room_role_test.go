package pgstore

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 建一個房間，回傳 public_id 和內部 id
func createRoleTestRoom(t *testing.T, s *DBStore, ownerID int64, name string) (string, int64) {
	t.Helper()
	room, err := s.CreateRoom(t.Context(), ownerID, name)
	if err != nil {
		t.Fatalf("setup CreateRoom: %v", err)
	}
	return room.PublicID, room.ID
}

// 直接讀 room_roles 的那一列（不經過 store 的 method）；found = false 代表沒有那一列
func readRoleRow(t *testing.T, s *DBStore, roomID, userID int64) (role string, assignedBy *int64, found bool) {
	t.Helper()
	rows, err := s.dbPool.Query(t.Context(), `SELECT role, assigned_by FROM room_roles WHERE room_id = $1 AND user_id = $2`, roomID, userID)
	if err != nil {
		t.Fatalf("read room_roles: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", nil, false
	}
	if err := rows.Scan(&role, &assignedBy); err != nil {
		t.Fatalf("scan room_roles: %v", err)
	}
	return role, assignedBy, true
}

// 預期 actor 把 target 改成 desired 的結果；store 回 error 就直接失敗
func mustChange(t *testing.T, s *DBStore, room string, actor, target int64, desired model.RoomRole) model.RoleChangeResult {
	t.Helper()
	res, err := s.ChangeRoomRole(t.Context(), room, actor, target, desired)
	if err != nil {
		t.Fatalf("ChangeRoomRole(actor %d, target %d, %s) error = %v, want nil", actor, target, desired, err)
	}
	return res
}

func contents(msgs []model.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func insertRawMessage(t *testing.T, s *DBStore, roomID int64, userID *int64, content string) {
	t.Helper()
	if _, err := s.dbPool.Exec(t.Context(), `INSERT INTO messages(room_id, user_id, content) VALUES ($1, $2, $3)`, roomID, userID, content); err != nil {
		t.Fatalf("setup insert message %q: %v", content, err)
	}
}

// NULL trap：user_id 是 NULL 的訊息（舊資料、已刪除的發送者）在房間出現第一個 ban 之後必須還看得到
// NOT IN / <> ALL 在這裡會變成 unknown，整批 NULL 訊息消失
func TestQueryMessageKeepsNullUserWhenSomeoneIsBanned(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	troll := createTestUser(t, s, "troll")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	insertRawMessage(t, s, roomID, nil, "legacy-anonymous")
	insertRawMessage(t, s, roomID, &troll, "troll-1")

	mustChange(t, s, room, owner, troll, model.RoomRoleBanned)

	msgs, err := s.QueryMessage(t.Context(), roomID, 50)
	if err != nil {
		t.Fatalf("QueryMessage error = %v", err)
	}
	if got, want := contents(msgs), []string{"legacy-anonymous"}; !slices.Equal(got, want) {
		t.Errorf("visible = %q, want %q", got, want)
	}
}

func TestQueryMessageHidesBannedAndFillsLimit(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	troll := createTestUser(t, s, "troll")
	fan := createTestUser(t, s, "fan")
	mod := createTestUser(t, s, "mod")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	// 房管不是 banned：他的訊息在 GET 必須看得到（過濾條件只能擋 role = 'banned'，不能擋所有有角色的人）
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	insertRawMessage(t, s, roomID, &mod, "mod-1")
	// 舊 → 新：mod-1, fan-1, troll-1, fan-2, troll-2, troll-3, fan-3
	insertRawMessage(t, s, roomID, &fan, "fan-1")
	insertRawMessage(t, s, roomID, &troll, "troll-1")
	insertRawMessage(t, s, roomID, &fan, "fan-2")
	insertRawMessage(t, s, roomID, &troll, "troll-2")
	insertRawMessage(t, s, roomID, &troll, "troll-3")
	insertRawMessage(t, s, roomID, &fan, "fan-3")
	mustChange(t, s, room, owner, troll, model.RoomRoleBanned)

	// limit 2：最新的 2 則「看得到的」，不是最新 2 列再過濾
	msgs, err := s.QueryMessage(t.Context(), roomID, 2)
	if err != nil {
		t.Fatalf("QueryMessage error = %v", err)
	}
	if got, want := contents(msgs), []string{"fan-3", "fan-2"}; !slices.Equal(got, want) {
		t.Errorf("limit 2 = %q, want %q", got, want)
	}
	// limit 10：全部看得到的
	msgs, _ = s.QueryMessage(t.Context(), roomID, 10)
	if got, want := contents(msgs), []string{"fan-3", "fan-2", "fan-1", "mod-1"}; !slices.Equal(got, want) {
		t.Errorf("limit 10 = %q, want %q", got, want)
	}

	// 被 ban 期間發的訊息，解除後也全部回來
	if _, banned, err := s.CreateMessage(t.Context(), room, troll, "troll-during-ban"); err != nil || !banned {
		t.Fatalf("CreateMessage during ban = banned %v, err %v, want banned true", banned, err)
	}
	mustChange(t, s, room, owner, troll, model.RoomRoleViewer)
	msgs, _ = s.QueryMessage(t.Context(), roomID, 50)
	want := []string{"troll-during-ban", "fan-3", "troll-3", "troll-2", "fan-2", "troll-1", "fan-1", "mod-1"}
	if got := contents(msgs); !slices.Equal(got, want) {
		t.Errorf("after unban = %q, want %q", got, want)
	}
}

func TestBanInOneRoomDoesNotHideInAnother(t *testing.T) {
	s := newTestDBStore(t)
	ownerA := createTestUser(t, s, "ownera")
	ownerB := createTestUser(t, s, "ownerb")
	troll := createTestUser(t, s, "troll")
	roomA, roomAID := createRoleTestRoom(t, s, ownerA, "a")
	_, roomBID := createRoleTestRoom(t, s, ownerB, "b")
	insertRawMessage(t, s, roomAID, &troll, "in-a")
	insertRawMessage(t, s, roomBID, &troll, "in-b")

	mustChange(t, s, roomA, ownerA, troll, model.RoomRoleBanned)

	a, _ := s.QueryMessage(t.Context(), roomAID, 50)
	b, _ := s.QueryMessage(t.Context(), roomBID, 50)
	if len(a) != 0 {
		t.Errorf("room A visible = %q, want none", contents(a))
	}
	if got, want := contents(b), []string{"in-b"}; !slices.Equal(got, want) {
		t.Errorf("room B visible = %q, want %q", got, want)
	}
}

func TestCreateMessageBannedFlag(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	troll := createTestUser(t, s, "troll")
	other := createTestUser(t, s, "other")
	mod := createTestUser(t, s, "mod")
	room, _ := createRoleTestRoom(t, s, owner, "r")
	otherRoom, _ := createRoleTestRoom(t, s, other, "o")
	// 房管有 room_roles 那一列但不是 banned：不能被 shadow ban
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	if _, banned, err := s.CreateMessage(t.Context(), room, mod, "mod"); err != nil || banned {
		t.Fatalf("moderator message: banned %v err %v, want false nil", banned, err)
	}

	_, banned, err := s.CreateMessage(t.Context(), room, troll, "before")
	if err != nil || banned {
		t.Fatalf("before ban: banned %v err %v, want false nil", banned, err)
	}
	mustChange(t, s, room, owner, troll, model.RoomRoleBanned)
	msg, banned, err := s.CreateMessage(t.Context(), room, troll, "during")
	if err != nil || !banned {
		t.Fatalf("during ban: banned %v err %v, want true nil", banned, err)
	}
	if msg.ID <= 0 || msg.Content != "during" {
		t.Errorf("banned author's message must still be stored and returned, got %+v", msg)
	}
	// 別間房不受影響；別人不受影響
	if _, banned, _ := s.CreateMessage(t.Context(), otherRoom, troll, "elsewhere"); banned {
		t.Error("banned flag leaked to another room")
	}
	if _, banned, _ := s.CreateMessage(t.Context(), room, owner, "owner"); banned {
		t.Error("owner reported banned")
	}
	mustChange(t, s, room, owner, troll, model.RoomRoleViewer)
	if _, banned, _ := s.CreateMessage(t.Context(), room, troll, "after"); banned {
		t.Error("banned flag still true after unban")
	}
}

func TestRoleOperationsOnSoftDeletedRoom(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	troll := createTestUser(t, s, "troll")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	if _, err := s.dbPool.Exec(t.Context(), `UPDATE rooms SET deleted_at = now() WHERE id = $1`, roomID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ChangeRoomRole(t.Context(), room, owner, troll, model.RoomRoleBanned); !errors.Is(err, model.ErrRoomNotFound) {
		t.Errorf("ChangeRoomRole error = %v, want ErrRoomNotFound", err)
	}
	if _, err := s.GetUserRoomRole(t.Context(), room, owner); !errors.Is(err, model.ErrRoomNotFound) {
		t.Errorf("GetUserRoomRole error = %v, want ErrRoomNotFound", err)
	}
	if _, _, err := s.CreateMessage(t.Context(), room, troll, "x"); !errors.Is(err, model.ErrRoomNotFound) {
		t.Errorf("CreateMessage error = %v, want ErrRoomNotFound", err)
	}
	if _, err := s.ChangeRoomRole(t.Context(), "nope0000000", owner, troll, model.RoomRoleBanned); !errors.Is(err, model.ErrRoomNotFound) {
		t.Errorf("missing room error = %v, want ErrRoomNotFound", err)
	}
}

// 順序：先看操作者，一般觀眾（或被 ban 的人）就算指定不存在的 user 也只會拿到 Forbidden，不能拿來探測 user
func TestChangeRoomRoleChecksActorBeforeTarget(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	viewer := createTestUser(t, s, "viewer")
	banned := createTestUser(t, s, "banned")
	room, _ := createRoleTestRoom(t, s, owner, "r")
	mustChange(t, s, room, owner, banned, model.RoomRoleBanned)
	const ghost = int64(987654321)

	for name, actor := range map[string]int64{"viewer": viewer, "banned": banned} {
		res, err := s.ChangeRoomRole(t.Context(), room, actor, ghost, model.RoomRoleBanned)
		if err != nil || res.Decision != model.RoleChangeForbidden {
			t.Errorf("%s with nonexistent target = (%v, %v), want Forbidden and nil error", name, res.Decision, err)
		}
	}
	// 有權限的人指定不存在的 user 才是 ErrUserNotFound
	if _, err := s.ChangeRoomRole(t.Context(), room, owner, ghost, model.RoomRoleBanned); !errors.Is(err, model.ErrUserNotFound) {
		t.Errorf("owner with nonexistent target error = %v, want ErrUserNotFound", err)
	}
}

func TestChangeRoomRoleTransitions(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	mod := createTestUser(t, s, "mod")
	mod2 := createTestUser(t, s, "mod2")
	troll := createTestUser(t, s, "troll")
	room, roomID := createRoleTestRoom(t, s, owner, "r")

	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	mustChange(t, s, room, owner, mod2, model.RoomRoleModerator)

	// 房管 ban 一般觀眾：新 ban，assigned_by = 房管
	res := mustChange(t, s, room, mod, troll, model.RoomRoleBanned)
	if res.Decision != model.RoleChangeAllow || !res.NewlyBanned {
		t.Fatalf("mod ban = %+v, want Allow + NewlyBanned", res)
	}
	if _, by, _ := readRoleRow(t, s, roomID, troll); by == nil || *by != mod {
		t.Fatalf("assigned_by = %v, want mod %d", by, mod)
	}
	// 房主 ban 同一個人：接手 assigned_by，但不算新 ban
	var createdBefore time.Time
	readCreatedAt := func() time.Time {
		var c time.Time
		if err := s.dbPool.QueryRow(t.Context(), `SELECT created_at FROM room_roles WHERE room_id = $1 AND user_id = $2`, roomID, troll).Scan(&c); err != nil {
			t.Fatalf("read created_at: %v", err)
		}
		return c
	}
	createdBefore = readCreatedAt()
	res = mustChange(t, s, room, owner, troll, model.RoomRoleBanned)
	if res.Decision != model.RoleChangeAllow || res.NewlyBanned {
		t.Fatalf("owner re-ban = %+v, want Allow without NewlyBanned", res)
	}
	if _, by, _ := readRoleRow(t, s, roomID, troll); by == nil || *by != owner {
		t.Fatalf("assigned_by after takeover = %v, want owner %d", by, owner)
	}
	// 接手要更新 created_at：名單排序和游標都靠它
	if after := readCreatedAt(); !after.After(createdBefore) {
		t.Errorf("created_at after takeover = %v, want later than %v", after, createdBefore)
	}
	// 房管重複 ban：Noop，不覆蓋房主
	res = mustChange(t, s, room, mod, troll, model.RoomRoleBanned)
	if res.Decision != model.RoleChangeNoop || res.NewlyBanned {
		t.Fatalf("mod re-ban = %+v, want Noop", res)
	}
	if _, by, _ := readRoleRow(t, s, roomID, troll); by == nil || *by != owner {
		t.Fatalf("assigned_by after mod re-ban = %v, want still owner %d", by, owner)
	}
	// 房管不能解房主 ban 的
	if res = mustChange(t, s, room, mod, troll, model.RoomRoleViewer); res.Decision != model.RoleChangeTargetForbidden {
		t.Fatalf("mod unban owner's ban = %v, want TargetForbidden", res.Decision)
	}
	if _, _, found := readRoleRow(t, s, roomID, troll); !found {
		t.Fatal("row deleted despite TargetForbidden")
	}
	// 指派被 ban 的人 → Conflict
	if res = mustChange(t, s, room, owner, troll, model.RoomRoleModerator); res.Decision != model.RoleChangeConflict {
		t.Fatalf("assign banned = %v, want Conflict", res.Decision)
	}
	// 房主解除 → 沒有那一列
	if res = mustChange(t, s, room, owner, troll, model.RoomRoleViewer); res.Decision != model.RoleChangeAllow || res.NewlyBanned {
		t.Fatalf("owner unban = %+v", res)
	}
	if _, _, found := readRoleRow(t, s, roomID, troll); found {
		t.Fatal("row still exists after unban")
	}
	// 解除一個本來就沒有角色的人：Noop
	if res = mustChange(t, s, room, owner, troll, model.RoomRoleViewer); res.Decision != model.RoleChangeNoop {
		t.Fatalf("unban viewer = %v, want Noop", res.Decision)
	}
	// 房管不能動房管、不能動房主、不能指派房管
	if res = mustChange(t, s, room, mod, mod2, model.RoomRoleBanned); res.Decision != model.RoleChangeTargetForbidden {
		t.Errorf("mod ban mod = %v, want TargetForbidden", res.Decision)
	}
	if res = mustChange(t, s, room, mod, owner, model.RoomRoleBanned); res.Decision != model.RoleChangeTargetForbidden {
		t.Errorf("mod ban owner = %v, want TargetForbidden", res.Decision)
	}
	if res = mustChange(t, s, room, mod, troll, model.RoomRoleModerator); res.Decision != model.RoleChangeForbidden {
		t.Errorf("mod assign mod = %v, want Forbidden", res.Decision)
	}
	// 房主 ban 房管：那一列改成 banned
	if res = mustChange(t, s, room, owner, mod2, model.RoomRoleBanned); res.Decision != model.RoleChangeAllow || !res.NewlyBanned {
		t.Errorf("owner ban mod = %+v, want Allow + NewlyBanned", res)
	}
	if role, _, _ := readRoleRow(t, s, roomID, mod2); role != "banned" {
		t.Errorf("role after owner bans mod = %q, want banned", role)
	}
	// 被 ban 的房管（現在是 banned）沒有權限
	if res = mustChange(t, s, room, mod2, troll, model.RoomRoleBanned); res.Decision != model.RoleChangeForbidden {
		t.Errorf("banned ex-mod ban = %v, want Forbidden", res.Decision)
	}
}

// assigned_by 是 NULL（指派者被刪除）不算房主 ban 的：房管可以解除
func TestChangeRoomRoleNullAssignedByIsNotOwners(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	mod := createTestUser(t, s, "mod")
	troll := createTestUser(t, s, "troll")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	mustChange(t, s, room, owner, troll, model.RoomRoleBanned)
	if _, err := s.dbPool.Exec(t.Context(), `UPDATE room_roles SET assigned_by = NULL WHERE room_id = $1 AND user_id = $2`, roomID, troll); err != nil {
		t.Fatal(err)
	}

	if res := mustChange(t, s, room, mod, troll, model.RoomRoleViewer); res.Decision != model.RoleChangeAllow {
		t.Errorf("mod unban NULL assigned_by = %v, want Allow", res.Decision)
	}
}

func TestGetUserRoomRole(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	mod := createTestUser(t, s, "mod")
	troll := createTestUser(t, s, "troll")
	viewer := createTestUser(t, s, "viewer")
	room, _ := createRoleTestRoom(t, s, owner, "r")
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	mustChange(t, s, room, owner, troll, model.RoomRoleBanned)

	want := map[int64]model.RoomRole{owner: model.RoomRoleOwner, mod: model.RoomRoleModerator, troll: model.RoomRoleBanned, viewer: model.RoomRoleViewer, 424242: model.RoomRoleViewer}
	for uid, w := range want {
		got, err := s.GetUserRoomRole(t.Context(), room, uid)
		if err != nil || got != w {
			t.Errorf("GetUserRoomRole(%d) = (%q, %v), want %q", uid, got, err, w)
		}
	}
}

func TestListRoomRoles(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	users := make([]int64, 5)
	for i := range users {
		users[i] = createTestUser(t, s, fmt.Sprintf("user%d", i))
	}
	// 固定時間：前三個同一時間（測 user_id tiebreak），後兩個依序更早
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{base, base, base, base.Add(-time.Hour), base.Add(-2 * time.Hour)}
	for i, u := range users {
		if _, err := s.dbPool.Exec(t.Context(),
			`INSERT INTO room_roles(room_id, user_id, role, assigned_by, created_at) VALUES ($1, $2, 'banned', $3, $4)`,
			roomID, u, owner, times[i]); err != nil {
			t.Fatal(err)
		}
	}
	// 指派者 NULL 的一列
	if _, err := s.dbPool.Exec(t.Context(), `UPDATE room_roles SET assigned_by = NULL WHERE room_id = $1 AND user_id = $2`, roomID, users[4]); err != nil {
		t.Fatal(err)
	}
	// 預期順序：created_at DESC，同時間 user_id DESC → users[2], users[1], users[0], users[3], users[4]
	wantOrder := []int64{users[2], users[1], users[0], users[3], users[4]}

	var got []int64
	var after *model.RoleCursor
	pages := 0
	for {
		entries, next, err := s.ListRoomRoles(t.Context(), room, after, 2)
		if err != nil {
			t.Fatalf("ListRoomRoles error = %v", err)
		}
		pages++
		for _, e := range entries {
			got = append(got, e.UserID)
			if e.UserID == users[4] && (e.AssignedBy != nil || e.AssignedByUsername != nil) {
				t.Errorf("NULL assigned_by entry = %+v, want nil assigned fields", e)
			}
			if e.UserID == users[0] && (e.AssignedBy == nil || *e.AssignedBy != owner || e.AssignedByUsername == nil || *e.AssignedByUsername != "owner") {
				t.Errorf("entry = %+v, want assigned_by owner", e)
			}
		}
		if next == nil {
			break
		}
		after = next
	}
	if !slices.Equal(got, wantOrder) {
		t.Errorf("order = %v, want %v", got, wantOrder)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if _, _, err := s.ListRoomRoles(t.Context(), room, nil, 0); !errors.Is(err, model.ErrInvalidLimit) {
		t.Errorf("limit 0 error = %v, want ErrInvalidLimit", err)
	}
}

// 真的 goroutine：房管 ban 和房主 ban 同時打同一個人，最後 assigned_by 一定是房主
// （沒有房間鎖時，房管的 upsert 可能在房主之後落地，把 assigned_by 蓋回房管）
func TestChangeRoomRoleModBanRacingOwnerBan(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	mod := createTestUser(t, s, "mod")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)

	for i := range 40 {
		target := createTestUser(t, s, fmt.Sprintf("target%d", i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, actor := range []int64{mod, owner} {
			wg.Go(func() {
				<-start
				_, err := s.ChangeRoomRole(t.Context(), room, actor, target, model.RoomRoleBanned)
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: ChangeRoomRole error = %v", i, err)
			}
		}
		role, by, found := readRoleRow(t, s, roomID, target)
		if !found || role != "banned" || by == nil || *by != owner {
			t.Fatalf("iteration %d: row = (%q, %v, found %v), want banned assigned by owner %d", i, role, by, found, owner)
		}
	}
}

// 大量同房間、同對象的混合變更同時進行：不能 deadlock、不能有 error（包含 PK 衝突）
func TestChangeRoomRoleConcurrentMixedChanges(t *testing.T) {
	s := newTestDBStore(t)
	owner := createTestUser(t, s, "owner")
	mod := createTestUser(t, s, "mod")
	room, roomID := createRoleTestRoom(t, s, owner, "r")
	mustChange(t, s, room, owner, mod, model.RoomRoleModerator)
	targets := []int64{createTestUser(t, s, "t0"), createTestUser(t, s, "t1")}

	desiredList := []model.RoomRole{model.RoomRoleBanned, model.RoomRoleViewer, model.RoomRoleModerator}
	done := make(chan struct{})
	errs := make(chan error, 200)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for j := range 25 {
				actor := []int64{owner, mod}[(g+j)%2]
				target := targets[(g+j/2)%2]
				if _, err := s.ChangeRoomRole(t.Context(), room, actor, target, desiredList[(g*7+j)%3]); err != nil {
					errs <- err
				}
				// 同時有訊息在寫入同一個房間：鎖不能擋 messages 的 FK 檢查
				if _, _, err := s.CreateMessage(t.Context(), room, target, "x"); err != nil {
					errs <- err
				}
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: concurrent role changes did not finish in 30s")
	}
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op error = %v", err)
	}
	for _, tg := range targets {
		role, _, found := readRoleRow(t, s, roomID, tg)
		if found && role != "banned" && role != "moderator" {
			t.Errorf("target %d role = %q", tg, role)
		}
	}
}
