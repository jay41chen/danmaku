package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 只為 sort=viewers 的測試：記錄 ListPopularRooms 的呼叫；其他方法不該被呼叫
type fakePopularUseCase struct {
	rooms []model.PopularRoom
	err   error
	calls int
	limit int
}

func (f *fakePopularUseCase) CreateRoom(ctx context.Context, ownerID int64, name string) (model.Room, error) {
	panic("unexpected")
}
func (f *fakePopularUseCase) GetRoom(ctx context.Context, publicID string) (model.Room, error) {
	panic("unexpected")
}
func (f *fakePopularUseCase) ListRooms(ctx context.Context, ownerID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error) {
	panic("unexpected: default path must not run for sort=viewers")
}
func (f *fakePopularUseCase) ListPopularRooms(ctx context.Context, limit int) ([]model.PopularRoom, error) {
	f.calls++
	f.limit = limit
	return f.rooms, f.err
}

func servePopular(uc *fakePopularUseCase, query string) *httptest.ResponseRecorder {
	h := NewRoomHandler(uc)
	rec := httptest.NewRecorder()
	h.HandleListRooms(rec, httptest.NewRequest(http.MethodGet, "/rooms?"+query, nil))
	return rec
}

func TestHandleListRoomsSortViewers(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		uc := &fakePopularUseCase{rooms: []model.PopularRoom{
			{Room: model.Room{Name: "大房", PublicID: "aaaaaaaaaaa", OwnerUsername: "x"}, ViewerCount: 12},
			{Room: model.Room{Name: "小房", PublicID: "bbbbbbbbbbb"}, ViewerCount: 3},
		}}
		rec := servePopular(uc, "sort=viewers&limit=7")
		want := `{"rooms":[{"room_name":"大房","public_id":"aaaaaaaaaaa","viewer_count":12},{"room_name":"小房","public_id":"bbbbbbbbbbb","viewer_count":3}]}` + "\n"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("status = %d body = %q, want 200 %q", rec.Code, rec.Body.String(), want)
		}
		if uc.limit != 7 {
			t.Errorf("limit = %d, want 7", uc.limit)
		}
	})

	t.Run("default limit 20 and empty rooms is []", func(t *testing.T) {
		uc := &fakePopularUseCase{}
		rec := servePopular(uc, "sort=viewers")
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"rooms\":[]}\n" {
			t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
		}
		if uc.limit != 20 {
			t.Errorf("limit = %d, want 20", uc.limit)
		}
	})

	invalid := []struct{ name, query, code string }{
		{"unknown sort", "sort=foo", "invalid_sort"},
		{"sort wrong case", "sort=Viewers", "invalid_sort"},
		{"page_token with sort=viewers", "sort=viewers&page_token=abc", "page_token_not_supported"},
		{"limit zero", "sort=viewers&limit=0", "invalid_limit"},
		{"limit too big", "sort=viewers&limit=51", "invalid_limit"},
		{"limit not a number", "sort=viewers&limit=x", "invalid_limit"},
		// 驗證順序：sort → limit → page_token
		{"bad sort wins over bad limit", "sort=foo&limit=0", "invalid_sort"},
		{"bad limit wins over page_token", "sort=viewers&limit=0&page_token=abc", "invalid_limit"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			uc := &fakePopularUseCase{}
			rec := servePopular(uc, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("body = %q, want code %s", rec.Body.String(), tc.code)
			}
			if uc.calls != 0 {
				t.Errorf("use case called %d times, want 0", uc.calls)
			}
		})
	}

	t.Run("ranking unavailable returns 503", func(t *testing.T) {
		uc := &fakePopularUseCase{err: errors.Join(model.ErrViewerRankingUnavailable, errors.New("x"))}
		rec := servePopular(uc, "sort=viewers")
		want := `{"error":{"code":"viewer_ranking_unavailable","message":"viewer ranking unavailable"}}` + "\n"
		if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != want {
			t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("other error returns 500", func(t *testing.T) {
		rec := servePopular(&fakePopularUseCase{err: errors.New("db")}, "sort=viewers")
		if rec.Code != http.StatusInternalServerError || rec.Body.String() != bodyInternalError {
			t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
		}
	})
}

// sort= 空字串 = 預設列表（走 ListRooms，不走 ListPopularRooms）
func TestHandleListRoomsEmptySortIsDefault(t *testing.T) {
	db := &fakeDBStore{listRooms: []model.Room{{Name: "房", PublicID: "aaaaaaaaaaa"}}}
	h := newTestRoomHandler(db)
	rec := httptest.NewRecorder()
	h.HandleListRooms(rec, httptest.NewRequest(http.MethodGet, "/rooms?sort=", nil))
	want := `{"rooms":[{"room_name":"房","public_id":"aaaaaaaaaaa"}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want || db.listRoomsCalls != 1 {
		t.Errorf("status = %d body = %q calls = %d", rec.Code, rec.Body.String(), db.listRoomsCalls)
	}
}
