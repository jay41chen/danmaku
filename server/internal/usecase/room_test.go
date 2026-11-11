package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 房間 use case 的假 store：記錄收到的參數和呼叫次數，回傳值由 test 設定
type fakeRoomStore struct {
	room    model.Room
	rooms   []model.Room
	next    *model.RoomCursor
	err     error
	listErr error

	createCalls  int
	gotOwnerID   int64
	gotName      string
	gotMaxRooms  int
	gotPublicID  string
	listCalls    int
	gotListOwner *int64
	gotListAfter *model.RoomCursor
	gotListLimit int
}

func (f *fakeRoomStore) CreateRoom(ctx context.Context, ownerID int64, name string, maxRooms int) (model.Room, error) {
	f.createCalls++
	f.gotOwnerID, f.gotName, f.gotMaxRooms = ownerID, name, maxRooms
	return f.room, f.err
}

func (f *fakeRoomStore) GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error) {
	f.gotPublicID = publicID
	return f.room, f.err
}

func (f *fakeRoomStore) GetRoomLists(ctx context.Context, userID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error) {
	f.listCalls++
	f.gotListOwner, f.gotListAfter, f.gotListLimit = userID, after, limit
	return f.rooms, f.next, f.listErr
}

func TestRoomCreateRoom(t *testing.T) {
	t.Run("invalid name returns CodedError and does not call store", func(t *testing.T) {
		cases := []struct {
			name, input, code string
		}{
			{"empty", "", "room_name_length"},
			{"leading space", " a", "room_name_surrounding_space"},
			{"control char", "a\x00b", "room_name_invalid"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store := &fakeRoomStore{}
				_, err := NewRoomUseCase(store).CreateRoom(t.Context(), 7, tc.input)

				var coded *model.CodedError
				if !errors.As(err, &coded) || coded.Code != tc.code {
					t.Fatalf("err = %v, want CodedError with code %q", err, tc.code)
				}
				if store.createCalls != 0 {
					t.Errorf("store called %d times, want 0", store.createCalls)
				}
			})
		}
	})

	t.Run("valid name calls store with original name and the limit", func(t *testing.T) {
		want := model.Room{PublicID: "abcDEF12345", Name: "我的房間"}
		store := &fakeRoomStore{room: want}

		got, err := NewRoomUseCase(store).CreateRoom(t.Context(), 7, "我的房間")

		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("room = %+v, want %+v", got, want)
		}
		if store.createCalls != 1 {
			t.Fatalf("store called %d times, want 1", store.createCalls)
		}
		if store.gotOwnerID != 7 || store.gotName != "我的房間" {
			t.Errorf("store got owner=%d name=%q, want 7 / 我的房間", store.gotOwnerID, store.gotName)
		}
		if store.gotMaxRooms != model.MaxRoomsPerUser {
			t.Errorf("maxRooms = %d, want %d", store.gotMaxRooms, model.MaxRoomsPerUser)
		}
	})

	t.Run("store errors are returned as-is", func(t *testing.T) {
		generic := errors.New("db down")
		for _, want := range []error{model.ErrRoomLimitReached, model.ErrRoomOwnerNotFound, generic} {
			t.Run(want.Error(), func(t *testing.T) {
				_, err := NewRoomUseCase(&fakeRoomStore{err: want}).CreateRoom(t.Context(), 7, "房間")
				if !errors.Is(err, want) {
					t.Errorf("err = %v, want %v", err, want)
				}
			})
		}
	})
}

func TestRoomGetRoom(t *testing.T) {
	t.Run("passes publicID and result through", func(t *testing.T) {
		want := model.Room{PublicID: "abcDEF12345", Name: "房間"}
		store := &fakeRoomStore{room: want}

		got, err := NewRoomUseCase(store).GetRoom(t.Context(), "abcDEF12345")

		if err != nil || got != want {
			t.Errorf("got (%+v, %v), want (%+v, nil)", got, err, want)
		}
		if store.gotPublicID != "abcDEF12345" {
			t.Errorf("publicID = %q, want abcDEF12345", store.gotPublicID)
		}
	})

	t.Run("passes errors through", func(t *testing.T) {
		_, err := NewRoomUseCase(&fakeRoomStore{err: model.ErrRoomNotFound}).GetRoom(t.Context(), "x")
		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Errorf("err = %v, want ErrRoomNotFound", err)
		}
	})
}

func TestRoomListRooms(t *testing.T) {
	owner := int64(7)
	after := &model.RoomCursor{PublicID: "aaaaaaaaaaa"}
	next := &model.RoomCursor{PublicID: "bbbbbbbbbbb"}
	rooms := []model.Room{{PublicID: "r1", Name: "一"}, {PublicID: "r2", Name: "二"}}

	cases := []struct {
		name  string
		owner *int64
		after *model.RoomCursor
		limit int
	}{
		{"no owner, first page", nil, nil, 20},
		{"owner and cursor", &owner, after, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRoomStore{rooms: rooms, next: next}

			gotRooms, gotNext, err := NewRoomUseCase(store).ListRooms(t.Context(), tc.owner, tc.after, tc.limit)

			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(gotRooms) != 2 || gotRooms[0] != rooms[0] || gotRooms[1] != rooms[1] || gotNext != next {
				t.Errorf("got (%+v, %+v), want (%+v, %+v)", gotRooms, gotNext, rooms, next)
			}
			if store.listCalls != 1 || store.gotListOwner != tc.owner || store.gotListAfter != tc.after || store.gotListLimit != tc.limit {
				t.Errorf("store got owner=%v after=%v limit=%d calls=%d, want %v / %v / %d / 1",
					store.gotListOwner, store.gotListAfter, store.gotListLimit, store.listCalls, tc.owner, tc.after, tc.limit)
			}
		})
	}

	t.Run("passes errors through", func(t *testing.T) {
		boom := errors.New("db down")
		_, _, err := NewRoomUseCase(&fakeRoomStore{listErr: boom}).ListRooms(t.Context(), nil, nil, 20)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
	})
}
