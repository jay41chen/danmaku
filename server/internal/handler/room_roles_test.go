package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 角色 handler 的假 store：依 actor 回不同結果，才測得到「被 ban 的人和一般觀眾回應一樣」
type fakeRoleStore struct {
	changeByActor map[int64]model.RoleChangeResult
	changeResult  model.RoleChangeResult
	changeErr     error
	changeCalls   int
	gotRoom       string
	gotActor      int64
	gotTarget     int64
	gotDesired    model.RoomRole

	roleByUser map[int64]model.RoomRole
	roleErr    error

	entries    []model.RoomRoleEntry
	listNext   *model.RoleCursor
	listErr    error
	listCalls  int
	gotAfter   *model.RoleCursor
	gotListLim int
}

func (f *fakeRoleStore) ChangeRoomRole(ctx context.Context, room string, actor, target int64, desired model.RoomRole) (model.RoleChangeResult, error) {
	f.changeCalls++
	f.gotRoom, f.gotActor, f.gotTarget, f.gotDesired = room, actor, target, desired
	if res, ok := f.changeByActor[actor]; ok {
		return res, f.changeErr
	}
	return f.changeResult, f.changeErr
}

func (f *fakeRoleStore) GetUserRoomRole(ctx context.Context, room string, userID int64) (model.RoomRole, error) {
	if f.roleErr != nil {
		return "", f.roleErr
	}
	if r, ok := f.roleByUser[userID]; ok {
		return r, nil
	}
	return model.RoomRoleViewer, nil
}

func (f *fakeRoleStore) ListRoomRoles(ctx context.Context, room string, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error) {
	f.listCalls++
	f.gotAfter, f.gotListLim = after, limit
	return f.entries, f.listNext, f.listErr
}

const testRoleRoom = "abcDEF12345"

func serveRole(t *testing.T, h http.HandlerFunc, method string, userID int64, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.SetPathValue("public_id", testRoleRoom)
	// path 形如 /rooms/x/roles/<id>：最後一段當 user_id（GET roles / me 沒有，值無意義）
	if i := strings.LastIndex(strings.SplitN(target, "?", 2)[0], "/"); i >= 0 {
		req.SetPathValue("user_id", strings.SplitN(target, "?", 2)[0][i+1:])
	}
	return serveAuthed(t, h, userID, req)
}

func TestSetRoleStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		store      fakeRoleStore
		path, body string
		wantStatus int
		wantCode   string
	}{
		{"allow", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeAllow}}, "/rooms/x/roles/5", `{"role":"moderator"}`, 204, ""},
		{"noop", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeNoop}}, "/rooms/x/roles/5", `{"role":"banned"}`, 204, ""},
		{"forbidden", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeForbidden}}, "/rooms/x/roles/5", `{"role":"banned"}`, 403, "room_forbidden"},
		{"target forbidden", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeTargetForbidden}}, "/rooms/x/roles/5", `{"role":"banned"}`, 403, "room_target_forbidden"},
		{"conflict", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeConflict}}, "/rooms/x/roles/5", `{"role":"moderator"}`, 409, "room_role_conflict"},
		{"user missing", fakeRoleStore{changeErr: model.ErrUserNotFound}, "/rooms/x/roles/5", `{"role":"banned"}`, 404, "user_not_found"},
		{"room missing", fakeRoleStore{changeErr: model.ErrRoomNotFound}, "/rooms/x/roles/5", `{"role":"banned"}`, 404, "room_not_found"},
		{"store failure", fakeRoleStore{changeErr: context.DeadlineExceeded}, "/rooms/x/roles/5", `{"role":"banned"}`, 500, "internal_error"},
		{"invalid json", fakeRoleStore{}, "/rooms/x/roles/5", `{nope`, 400, "invalid_json"},
		{"empty body", fakeRoleStore{}, "/rooms/x/roles/5", ``, 400, "invalid_json"},
		{"role owner", fakeRoleStore{}, "/rooms/x/roles/5", `{"role":"owner"}`, 400, "invalid_role"},
		{"role viewer", fakeRoleStore{}, "/rooms/x/roles/5", `{"role":"viewer"}`, 400, "invalid_role"},
		{"role unknown", fakeRoleStore{}, "/rooms/x/roles/5", `{"role":"admin"}`, 400, "invalid_role"},
		{"role missing", fakeRoleStore{}, "/rooms/x/roles/5", `{}`, 400, "invalid_role"},
		{"user id not numeric", fakeRoleStore{}, "/rooms/x/roles/abc", `{"role":"banned"}`, 404, "user_not_found"},
		{"user id zero", fakeRoleStore{}, "/rooms/x/roles/0", `{"role":"banned"}`, 404, "user_not_found"},
		{"user id negative", fakeRoleStore{}, "/rooms/x/roles/-3", `{"role":"banned"}`, 404, "user_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewRoleHandler(&tc.store, &fakeRedis{})

			rec := serveRole(t, h.HandleSetRole, http.MethodPut, 7, tc.path, tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				if got := decodeErrorBody(t, rec).Code; got != tc.wantCode {
					t.Errorf("code = %q, want %q", got, tc.wantCode)
				}
			} else if rec.Body.Len() != 0 {
				t.Errorf("204 body = %q, want empty", rec.Body.String())
			}
		})
	}
}

func TestSetRolePassesArgumentsToStore(t *testing.T) {
	store := &fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeAllow}}
	h := NewRoleHandler(store, &fakeRedis{})

	serveRole(t, h.HandleSetRole, http.MethodPut, 7, "/rooms/x/roles/5", `{"role":"banned"}`)
	if store.gotRoom != testRoleRoom || store.gotActor != 7 || store.gotTarget != 5 || store.gotDesired != model.RoomRoleBanned {
		t.Errorf("PUT store args = (%q, %d, %d, %q)", store.gotRoom, store.gotActor, store.gotTarget, store.gotDesired)
	}
	serveRole(t, h.HandleDeleteRole, http.MethodDelete, 7, "/rooms/x/roles/5", "")
	if store.gotDesired != model.RoomRoleViewer {
		t.Errorf("DELETE desired = %q, want viewer", store.gotDesired)
	}
}

func TestDeleteRoleStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		store      fakeRoleStore
		path       string
		wantStatus int
		wantCode   string
	}{
		{"allow", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeAllow}}, "/rooms/x/roles/5", 204, ""},
		{"noop", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeNoop}}, "/rooms/x/roles/5", 204, ""},
		{"forbidden", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeForbidden}}, "/rooms/x/roles/5", 403, "room_forbidden"},
		{"target forbidden", fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeTargetForbidden}}, "/rooms/x/roles/5", 403, "room_target_forbidden"},
		{"user missing", fakeRoleStore{changeErr: model.ErrUserNotFound}, "/rooms/x/roles/5", 404, "user_not_found"},
		{"room missing", fakeRoleStore{changeErr: model.ErrRoomNotFound}, "/rooms/x/roles/5", 404, "room_not_found"},
		{"bad user id", fakeRoleStore{}, "/rooms/x/roles/zero", 404, "user_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewRoleHandler(&tc.store, &fakeRedis{})
			rec := serveRole(t, h.HandleDeleteRole, http.MethodDelete, 7, tc.path, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantCode != "" && decodeErrorBody(t, rec).Code != tc.wantCode {
				t.Errorf("code mismatch, body %q", rec.Body.String())
			}
		})
	}
}

// 被 ban 的人和一般觀眾：PUT / DELETE / GET roles 的回應 byte 相同；/me 都回 viewer
func TestBannedAndViewerAreIndistinguishable(t *testing.T) {
	const viewerID, bannedID = int64(5), int64(6)
	store := &fakeRoleStore{
		changeByActor: map[int64]model.RoleChangeResult{
			viewerID: {Decision: model.RoleChangeForbidden},
			bannedID: {Decision: model.RoleChangeForbidden},
		},
		roleByUser: map[int64]model.RoomRole{bannedID: model.RoomRoleBanned},
	}
	h := NewRoleHandler(store, &fakeRedis{})

	same := func(name string, call func(userID int64) *httptest.ResponseRecorder) {
		t.Helper()
		v, b := call(viewerID), call(bannedID)
		if v.Code != b.Code || v.Body.String() != b.Body.String() || v.Header().Get("Cache-Control") != b.Header().Get("Cache-Control") {
			t.Errorf("%s differs: viewer (%d %q %q) vs banned (%d %q %q)", name,
				v.Code, v.Body.String(), v.Header().Get("Cache-Control"), b.Code, b.Body.String(), b.Header().Get("Cache-Control"))
		}
	}
	same("PUT", func(u int64) *httptest.ResponseRecorder {
		return serveRole(t, h.HandleSetRole, http.MethodPut, u, "/rooms/x/roles/9", `{"role":"banned"}`)
	})
	same("DELETE", func(u int64) *httptest.ResponseRecorder {
		return serveRole(t, h.HandleDeleteRole, http.MethodDelete, u, "/rooms/x/roles/9", "")
	})
	same("GET roles", func(u int64) *httptest.ResponseRecorder {
		return serveRole(t, h.HandleListRoles, http.MethodGet, u, "/rooms/x/roles", "")
	})
	same("GET me", func(u int64) *httptest.ResponseRecorder {
		return serveRole(t, h.HandleMyRole, http.MethodGet, u, "/rooms/x/me", "")
	})
	if rec := serveRole(t, h.HandleMyRole, http.MethodGet, bannedID, "/rooms/x/me", ""); rec.Body.String() != "{\"role\":\"viewer\"}\n" {
		t.Errorf("banned /me body = %q, want viewer", rec.Body.String())
	}
	if rec := serveRole(t, h.HandleListRoles, http.MethodGet, bannedID, "/rooms/x/roles", ""); rec.Code != 403 || decodeErrorBody(t, rec).Code != "room_forbidden" {
		t.Errorf("banned GET roles = %d %q, want 403 room_forbidden", rec.Code, rec.Body.String())
	}
	if store.listCalls != 0 {
		t.Error("ListRoomRoles was called for a viewer / banned user")
	}
}

func TestMyRole(t *testing.T) {
	store := &fakeRoleStore{roleByUser: map[int64]model.RoomRole{1: model.RoomRoleOwner, 2: model.RoomRoleModerator, 3: model.RoomRoleBanned}}
	h := NewRoleHandler(store, &fakeRedis{})
	for id, want := range map[int64]string{1: "owner", 2: "moderator", 3: "viewer", 4: "viewer"} {
		rec := serveRole(t, h.HandleMyRole, http.MethodGet, id, "/rooms/x/me", "")
		if rec.Code != 200 || rec.Body.String() != `{"role":"`+want+"\"}\n" {
			t.Errorf("user %d: %d %q, want role %s", id, rec.Code, rec.Body.String(), want)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("user %d Cache-Control = %q, want no-store", id, got)
		}
	}
	store.roleErr = model.ErrRoomNotFound
	rec := serveRole(t, h.HandleMyRole, http.MethodGet, 1, "/rooms/x/me", "")
	if rec.Code != 404 || decodeErrorBody(t, rec).Code != "room_not_found" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("missing room: %d %q cc %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
}

func TestListRoles(t *testing.T) {
	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	by, byName := int64(1), "owner"
	store := &fakeRoleStore{
		roleByUser: map[int64]model.RoomRole{1: model.RoomRoleOwner, 2: model.RoomRoleModerator},
		entries: []model.RoomRoleEntry{
			{UserID: 9, Username: "troll", Role: model.RoomRoleBanned, AssignedBy: &by, AssignedByUsername: &byName, CreatedAt: created},
			{UserID: 8, Username: "orphan", Role: model.RoomRoleBanned, CreatedAt: created},
		},
		listNext: &model.RoleCursor{CreatedAt: created, UserID: 8},
	}
	h := NewRoleHandler(store, &fakeRedis{})

	for _, actor := range []int64{1, 2} {
		rec := serveRole(t, h.HandleListRoles, http.MethodGet, actor, "/rooms/x/roles", "")
		if rec.Code != 200 {
			t.Fatalf("actor %d status = %d (%q)", actor, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		var resp struct {
			Roles         []map[string]any `json:"roles"`
			NextPageToken string           `json:"next_page_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want0 := `{"assigned_by":1,"assigned_by_username":"owner","created_at":"2030-01-02T03:04:05Z","role":"banned","user_id":9,"username":"troll"}`
		want1 := `{"assigned_by":null,"assigned_by_username":null,"created_at":"2030-01-02T03:04:05Z","role":"banned","user_id":8,"username":"orphan"}`
		for i, want := range []string{want0, want1} {
			got, _ := json.Marshal(resp.Roles[i]) // map 的 key 會排序，和 want 的字母序一致
			if string(got) != want {
				t.Errorf("roles[%d] = %s, want %s", i, got, want)
			}
		}
		cursor, err := decodeRoleToken(resp.NextPageToken)
		if err != nil || cursor.UserID != 8 || !cursor.CreatedAt.Equal(created) {
			t.Errorf("next_page_token decodes to (%+v, %v), want user 8 at %v", cursor, err, created)
		}
	}
	if store.gotListLim != defaultRoomListLimit {
		t.Errorf("default limit = %d, want %d", store.gotListLim, defaultRoomListLimit)
	}
}

func TestListRolesEmptyAndLastPage(t *testing.T) {
	store := &fakeRoleStore{roleByUser: map[int64]model.RoomRole{1: model.RoomRoleOwner}}
	h := NewRoleHandler(store, &fakeRedis{})
	rec := serveRole(t, h.HandleListRoles, http.MethodGet, 1, "/rooms/x/roles", "")
	if rec.Body.String() != "{\"roles\":[]}\n" {
		t.Errorf("empty body = %q, want {\"roles\":[]} (no null, no token)", rec.Body.String())
	}
}

func TestListRolesQueryValidation(t *testing.T) {
	store := &fakeRoleStore{roleByUser: map[int64]model.RoomRole{1: model.RoomRoleOwner}}
	h := NewRoleHandler(store, &fakeRedis{})
	roomTok, _ := encodePageToken(model.RoomCursor{CreatedAt: time.Now(), PublicID: testRoleRoom})
	roleTok, _ := encodeRoleToken(model.RoleCursor{CreatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), UserID: 4})

	for _, tc := range []struct{ name, query, code string }{
		{"limit zero", "?limit=0", "invalid_limit"},
		{"limit too big", "?limit=51", "invalid_limit"},
		{"limit empty", "?limit=", "invalid_limit"},
		{"garbage token", "?page_token=zzz", "invalid_cursor"},
		{"rooms token rejected", "?page_token=" + roomTok, "invalid_cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRole(t, h.HandleListRoles, http.MethodGet, 1, "/rooms/x/roles"+tc.query, "")
			if rec.Code != 400 || decodeErrorBody(t, rec).Code != tc.code {
				t.Errorf("got %d %q, want 400 %s", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
	if store.listCalls != 0 {
		t.Error("store was called despite invalid query")
	}

	rec := serveRole(t, h.HandleListRoles, http.MethodGet, 1, "/rooms/x/roles?limit=50&page_token="+roleTok, "")
	if rec.Code != 200 || store.gotListLim != 50 || store.gotAfter == nil || store.gotAfter.UserID != 4 {
		t.Errorf("valid query: %d limit %d after %+v", rec.Code, store.gotListLim, store.gotAfter)
	}
	// 反過來：角色名單的 token 不能拿去列房間
	if _, err := decodePageToken(roleTok); err == nil {
		t.Error("role token accepted by decodePageToken")
	}
}

func TestListRolesErrors(t *testing.T) {
	store := &fakeRoleStore{roleErr: model.ErrRoomNotFound}
	h := NewRoleHandler(store, &fakeRedis{})
	if rec := serveRole(t, h.HandleListRoles, http.MethodGet, 1, "/rooms/x/roles", ""); rec.Code != 404 || decodeErrorBody(t, rec).Code != "room_not_found" {
		t.Errorf("missing room: %d %q", rec.Code, rec.Body.String())
	}
	store.roleErr = nil
	store.roleByUser = map[int64]model.RoomRole{1: model.RoomRoleOwner}
	store.listErr = context.DeadlineExceeded
	if rec := serveRole(t, h.HandleListRoles, http.MethodGet, 1, "/rooms/x/roles", ""); rec.Code != 500 {
		t.Errorf("list failure status = %d, want 500", rec.Code)
	}
}

// ---- user_hidden 廣播 ----

const goldenUserHidden = `{"type":"user_hidden","payload":{"user_id":123}}`

func TestUserHiddenPublishedOnlyWhenNewlyBanned(t *testing.T) {
	cases := []struct {
		name        string
		res         model.RoleChangeResult
		err         error
		wantPublish bool
	}{
		{"newly banned", model.RoleChangeResult{Decision: model.RoleChangeAllow, NewlyBanned: true}, nil, true},
		{"owner takeover re-ban (allow, not new)", model.RoleChangeResult{Decision: model.RoleChangeAllow}, nil, false},
		{"noop", model.RoleChangeResult{Decision: model.RoleChangeNoop}, nil, false},
		{"forbidden", model.RoleChangeResult{Decision: model.RoleChangeForbidden}, nil, false},
		{"target forbidden", model.RoleChangeResult{Decision: model.RoleChangeTargetForbidden}, nil, false},
		{"conflict", model.RoleChangeResult{Decision: model.RoleChangeConflict}, nil, false},
		{"user not found", model.RoleChangeResult{}, model.ErrUserNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redis := &fakeRedis{}
			h := NewRoleHandler(&fakeRoleStore{changeResult: tc.res, changeErr: tc.err}, redis)

			serveRole(t, h.HandleSetRole, http.MethodPut, 7, "/rooms/x/roles/123", `{"role":"banned"}`)

			if !tc.wantPublish {
				if len(redis.published) != 0 {
					t.Fatalf("published %q, want nothing", redis.published)
				}
				return
			}
			if len(redis.published) != 1 || redis.published[0] != goldenUserHidden || redis.publishedRooms[0] != testRoleRoom {
				t.Errorf("published = %q rooms %q, want one %s to %s", redis.published, redis.publishedRooms, goldenUserHidden, testRoleRoom)
			}
		})
	}
}

// DELETE（解除 ban）即使 store 回 NewlyBanned 也不會廣播的前提是 store 不會那樣回；handler 只看 NewlyBanned，
// 所以這裡確認 DELETE 的 Allow 不廣播
func TestDeleteRoleDoesNotPublish(t *testing.T) {
	redis := &fakeRedis{}
	h := NewRoleHandler(&fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeAllow}}, redis)
	serveRole(t, h.HandleDeleteRole, http.MethodDelete, 7, "/rooms/x/roles/123", "")
	if len(redis.published) != 0 {
		t.Errorf("published %q on unban", redis.published)
	}
}

func TestUserHiddenPublishFailureStill204(t *testing.T) {
	redis := &fakeRedis{publishErr: context.DeadlineExceeded}
	h := NewRoleHandler(&fakeRoleStore{changeResult: model.RoleChangeResult{Decision: model.RoleChangeAllow, NewlyBanned: true}}, redis)
	before := counterValue(t, prom.PublishFailedTotal)

	rec := serveRole(t, h.HandleSetRole, http.MethodPut, 7, "/rooms/x/roles/123", `{"role":"banned"}`)

	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if len(redis.published) != 1 {
		t.Errorf("publish attempts = %d, want exactly 1 (never retried)", len(redis.published))
	}
	if got := counterValue(t, prom.PublishFailedTotal); got != before+1 {
		t.Errorf("PublishFailedTotal = %v, want %v", got, before+1)
	}
}
