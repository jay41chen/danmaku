package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// use case 的假 store：可以設定回傳值；decideRoles 有設的話會真的呼叫收到的 decide，
// 才驗得到 use case 傳進來的是不是 model.DecideRoleChange
type fakeStore struct {
	changeResult model.RoleChangeResult
	changeErr    error
	changeCalls  int
	gotRoom      string
	gotActor     int64
	gotTarget    int64
	gotDesired   model.RoomRole

	// 用 decide 算結果時的假資料
	useDecide     bool
	actorRole     model.RoomRole
	targetRole    model.RoomRole
	bannedByOwner bool

	role     model.RoomRole
	roleErr  error
	entries  []model.RoomRoleEntry
	next     *model.RoleCursor
	listErr  error
	listCall int
	gotAfter *model.RoleCursor
	gotLimit int
}

func (f *fakeStore) ChangeRoomRole(ctx context.Context, room string, actor, target int64, desired model.RoomRole, decide model.RoleDecider) (model.RoleChangeResult, error) {
	f.changeCalls++
	f.gotRoom, f.gotActor, f.gotTarget, f.gotDesired = room, actor, target, desired
	if f.changeErr != nil {
		return model.RoleChangeResult{}, f.changeErr
	}
	if f.useDecide {
		return model.RoleChangeResult{Decision: decide(f.actorRole, f.targetRole, desired, f.bannedByOwner)}, nil
	}
	return f.changeResult, nil
}

func (f *fakeStore) GetUserRoomRole(ctx context.Context, room string, userID int64) (model.RoomRole, error) {
	return f.role, f.roleErr
}

func (f *fakeStore) ListRoomRoles(ctx context.Context, room string, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error) {
	f.listCall++
	f.gotAfter, f.gotLimit = after, limit
	return f.entries, f.next, f.listErr
}

type hiddenCall struct {
	room string
	user int64
}

type fakeNotifier struct{ calls []hiddenCall }

func (f *fakeNotifier) NotifyUserHidden(ctx context.Context, room string, userID int64) {
	f.calls = append(f.calls, hiddenCall{room, userID})
}

const (
	testRoom = "room0000001"
	actorID  = int64(11)
	targetID = int64(22)
)

var modelErrs = []error{model.ErrRoomForbidden, model.ErrRoomTargetForbidden, model.ErrRoomRoleConflict, model.ErrInvalidRole}

// desired 不是 moderator / banned / viewer 就回 ErrInvalidRole，而且不能碰到 store
// （owner 只來自 rooms.owner_id，不能被指派）
func TestChangeRole_InvalidDesired(t *testing.T) {
	for _, desired := range []model.RoomRole{model.RoomRoleOwner, "", "admin"} {
		t.Run(string(desired), func(t *testing.T) {
			store, n := &fakeStore{}, &fakeNotifier{}
			err := NewRoleUseCase(store, n).ChangeRole(t.Context(), testRoom, actorID, targetID, desired)
			if !errors.Is(err, model.ErrInvalidRole) {
				t.Fatalf("err = %v, want ErrInvalidRole", err)
			}
			if store.changeCalls != 0 || len(n.calls) != 0 {
				t.Errorf("store calls = %d, notified = %d, want 0 / 0", store.changeCalls, len(n.calls))
			}
		})
	}
}

// 每個 Decision 只能對到自己的 error；Allow / Noop 是 nil；不認得的值要報錯，不能當成功
func TestChangeRole_DecisionMapping(t *testing.T) {
	cases := []struct {
		name     string
		decision model.RoleDecision
		want     error // nil 代表成功
	}{
		{"forbidden", model.RoleChangeForbidden, model.ErrRoomForbidden},
		{"target forbidden", model.RoleChangeTargetForbidden, model.ErrRoomTargetForbidden},
		{"conflict", model.RoleChangeConflict, model.ErrRoomRoleConflict},
		{"allow", model.RoleChangeAllow, nil},
		{"noop", model.RoleChangeNoop, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{changeResult: model.RoleChangeResult{Decision: tc.decision}}
			err := NewRoleUseCase(store, &fakeNotifier{}).ChangeRole(t.Context(), testRoom, actorID, targetID, model.RoomRoleBanned)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			for _, e := range modelErrs {
				if got := errors.Is(err, e); got != (e == tc.want) {
					t.Errorf("errors.Is(err, %v) = %v, err = %v", e, got, err)
				}
			}
		})
	}

	t.Run("unknown", func(t *testing.T) {
		store := &fakeStore{changeResult: model.RoleChangeResult{Decision: model.RoleDecision(99)}}
		err := NewRoleUseCase(store, &fakeNotifier{}).ChangeRole(t.Context(), testRoom, actorID, targetID, model.RoomRoleBanned)
		if err == nil {
			t.Fatal("err = nil, want error for unknown decision")
		}
		for _, e := range modelErrs {
			if errors.Is(err, e) {
				t.Errorf("unknown decision mapped to %v", e)
			}
		}
	})
}

// store 出錯（例如房間不存在）要原樣回傳，而且不能通知
func TestChangeRole_StoreError(t *testing.T) {
	store := &fakeStore{changeErr: model.ErrRoomNotFound, changeResult: model.RoleChangeResult{NewlyBanned: true}}
	n := &fakeNotifier{}
	err := NewRoleUseCase(store, n).ChangeRole(t.Context(), testRoom, actorID, targetID, model.RoomRoleBanned)
	if !errors.Is(err, model.ErrRoomNotFound) {
		t.Fatalf("err = %v, want ErrRoomNotFound", err)
	}
	if len(n.calls) != 0 {
		t.Errorf("notified %v on store error", n.calls)
	}
}

// 傳給 store 的規則必須是真正的 DecideRoleChange：假 store 會呼叫它，
// 若換成永遠放行的規則，房管解除房主 ban 的那個 case 就會變成 nil 而失敗
func TestChangeRole_UsesDecideRoleChange(t *testing.T) {
	cases := []struct {
		name          string
		actor, target model.RoomRole
		bannedByOwner bool
		desired       model.RoomRole
		want          error
	}{
		{"moderator cannot unban owner's ban", model.RoomRoleModerator, model.RoomRoleBanned, true, model.RoomRoleViewer, model.ErrRoomTargetForbidden},
		{"owner promotes viewer", model.RoomRoleOwner, model.RoomRoleViewer, false, model.RoomRoleModerator, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{useDecide: true, actorRole: tc.actor, targetRole: tc.target, bannedByOwner: tc.bannedByOwner}
			err := NewRoleUseCase(store, &fakeNotifier{}).ChangeRole(t.Context(), testRoom, actorID, targetID, tc.desired)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// 只有 Allow 且新被 ban 才通知，而且通知的是被 ban 的人（不是操作者）；
// 錯誤要先回，不能因為 NewlyBanned 就通知
func TestChangeRole_Notify(t *testing.T) {
	cases := []struct {
		name   string
		result model.RoleChangeResult
		want   int
	}{
		{"allow newly banned", model.RoleChangeResult{Decision: model.RoleChangeAllow, NewlyBanned: true}, 1},
		{"allow not newly banned", model.RoleChangeResult{Decision: model.RoleChangeAllow}, 0},
		{"noop", model.RoleChangeResult{Decision: model.RoleChangeNoop}, 0},
		{"forbidden with newly banned", model.RoleChangeResult{Decision: model.RoleChangeForbidden, NewlyBanned: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, n := &fakeStore{changeResult: tc.result}, &fakeNotifier{}
			_ = NewRoleUseCase(store, n).ChangeRole(t.Context(), testRoom, actorID, targetID, model.RoomRoleBanned)
			if len(n.calls) != tc.want {
				t.Fatalf("notify calls = %v, want %d", n.calls, tc.want)
			}
			if tc.want == 1 && n.calls[0] != (hiddenCall{testRoom, targetID}) {
				t.Errorf("notified %+v, want {%s %d}", n.calls[0], testRoom, targetID)
			}
			if store.gotRoom != testRoom || store.gotActor != actorID || store.gotTarget != targetID || store.gotDesired != model.RoomRoleBanned {
				t.Errorf("store got %q %d %d %q", store.gotRoom, store.gotActor, store.gotTarget, store.gotDesired)
			}
		})
	}
}

// 自己的角色：被 ban 要回 viewer（shadow ban 不能露餡），其他原樣；store 錯誤回空字串和原錯誤
func TestMyRole(t *testing.T) {
	cases := []struct {
		store model.RoomRole
		want  model.RoomRole
	}{
		{model.RoomRoleBanned, model.RoomRoleViewer},
		{model.RoomRoleOwner, model.RoomRoleOwner},
		{model.RoomRoleModerator, model.RoomRoleModerator},
		{model.RoomRoleViewer, model.RoomRoleViewer},
	}
	for _, tc := range cases {
		t.Run(string(tc.store), func(t *testing.T) {
			got, err := NewRoleUseCase(&fakeStore{role: tc.store}, &fakeNotifier{}).MyRole(t.Context(), testRoom, actorID)
			if err != nil || got != tc.want {
				t.Errorf("MyRole = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}

	t.Run("store error", func(t *testing.T) {
		got, err := NewRoleUseCase(&fakeStore{role: model.RoomRoleOwner, roleErr: model.ErrRoomNotFound}, &fakeNotifier{}).MyRole(t.Context(), testRoom, actorID)
		if got != "" || !errors.Is(err, model.ErrRoomNotFound) {
			t.Errorf("MyRole = %q, %v; want \"\", ErrRoomNotFound", got, err)
		}
	})
}

// 房主和房管可以列名單（參數和結果原樣傳遞）；一般觀眾和被 ban 的人拿到同樣的 ErrRoomForbidden 且不查名單；
// 查角色失敗直接回傳
func TestListRoles(t *testing.T) {
	entries := []model.RoomRoleEntry{{UserID: 5, Username: "a"}}
	next := &model.RoleCursor{UserID: 5}
	after := &model.RoleCursor{UserID: 3}

	for _, role := range []model.RoomRole{model.RoomRoleOwner, model.RoomRoleModerator} {
		t.Run("allowed "+string(role), func(t *testing.T) {
			store := &fakeStore{role: role, entries: entries, next: next, listErr: errors.New("list boom")}
			gotE, gotN, err := NewRoleUseCase(store, &fakeNotifier{}).ListRoles(t.Context(), testRoom, actorID, after, 30)
			if store.listCall != 1 || store.gotAfter != after || store.gotLimit != 30 {
				t.Errorf("list calls = %d after = %v limit = %d", store.listCall, store.gotAfter, store.gotLimit)
			}
			if len(gotE) != 1 || gotE[0].UserID != 5 || gotN != next || err == nil || err.Error() != "list boom" {
				t.Errorf("got %v %v %v, want passthrough", gotE, gotN, err)
			}
		})
	}

	for _, role := range []model.RoomRole{model.RoomRoleViewer, model.RoomRoleBanned} {
		t.Run("denied "+string(role), func(t *testing.T) {
			store := &fakeStore{role: role, entries: entries, next: next}
			gotE, gotN, err := NewRoleUseCase(store, &fakeNotifier{}).ListRoles(t.Context(), testRoom, actorID, after, 30)
			if !errors.Is(err, model.ErrRoomForbidden) || gotE != nil || gotN != nil {
				t.Errorf("got %v %v %v, want nil, nil, ErrRoomForbidden", gotE, gotN, err)
			}
			if store.listCall != 0 {
				t.Errorf("list called %d times", store.listCall)
			}
		})
	}

	t.Run("role lookup error", func(t *testing.T) {
		store := &fakeStore{role: model.RoomRoleOwner, roleErr: model.ErrRoomNotFound}
		_, _, err := NewRoleUseCase(store, &fakeNotifier{}).ListRoles(t.Context(), testRoom, actorID, nil, 30)
		if !errors.Is(err, model.ErrRoomNotFound) || store.listCall != 0 {
			t.Errorf("err = %v, list calls = %d", err, store.listCall)
		}
	})
}
