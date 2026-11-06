package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 同 serveCreateRoomMessage：用 RequireAuth 包起來送 Bearer token，user id 才會進到 ctx
func serveCreateRoom(t *testing.T, h *RoomHandler, userID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	token, _, err := testTokens.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	wrapped := middleware.RequireAuth(testTokens)(http.HandlerFunc(h.HandleCreateRoom))
	req := httptest.NewRequest(http.MethodPost, "/rooms", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

func roomNameBody(name string) string {
	b, _ := json.Marshal(map[string]string{"room_name": name})
	return string(b)
}

func TestHandleCreateRoom(t *testing.T) {
	const userID = int64(7)
	created := func() model.Room {
		return model.Room{ID: 5, OwnerID: userID, PublicID: "abcDEF12345", OwnerUsername: "Jay", Name: "我的房間",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	}

	t.Run("success returns 201 with public fields only", func(t *testing.T) {
		db := &fakeDBStore{createdRoom: created()}
		h := NewRoomHandler(db)

		rec := serveCreateRoom(t, h, userID, roomNameBody("我的房間"))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if db.gotRoomOwnerID != userID || db.gotRoomName != "我的房間" {
			t.Errorf("CreateRoom got (%d, %q), want (%d, %q)", db.gotRoomOwnerID, db.gotRoomName, userID, "我的房間")
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["public_id"] != "abcDEF12345" || body["room_name"] != "我的房間" || body["owner_username"] != "Jay" {
			t.Errorf("body = %v, want public_id / room_name / owner_username of the created room", body)
		}
		// 內部 id 不能外洩
		for _, key := range []string{"id", "ID", "owner_id", "OwnerID"} {
			if _, ok := body[key]; ok {
				t.Errorf("body contains internal field %q: %v", key, body)
			}
		}
	})

	// 驗證失敗的 case：fake DB 不會被呼叫到
	invalid := []struct {
		name string
		body string
	}{
		{"broken json", `{"room_name":`},
		{"missing room_name", `{}`},
		{"empty name", roomNameBody("")},
		{"whitespace only", roomNameBody("  \t\n")},
		{"full-width space only", roomNameBody("　　")},
		{"31 runes", roomNameBody(strings.Repeat("哈", 31))},
		// precis Freeform 擋掉的字元
		{"zero-width space", roomNameBody("\u200b房間")},
		{"right-to-left override", roomNameBody("abc\u202eevil")},
		{"newline", roomNameBody("房\n間")},
		{"ZWJ family emoji", roomNameBody("\U0001F468\u200d\U0001F469\u200d\U0001F467")},
		// 頭尾不可有空白（不再 trim）
		{"leading space", roomNameBody(" 房間")},
		{"trailing space", roomNameBody("房間 ")},
		{"trailing full-width space", roomNameBody("房間\u3000")},
		// 相鄰兩個空白
		{"two adjacent ASCII spaces", roomNameBody("房  間")},
		{"ASCII space + full-width space", roomNameBody("房 \u3000間")},
		// 連續超過 2 個組合記號
		{"3 consecutive combining marks", roomNameBody("Z\u0301\u0301\u0301")},
		{"3 combining marks at start", roomNameBody("\u0301\u0301\u0301房")},
	}
	for _, tc := range invalid {
		t.Run(tc.name+" returns 400", func(t *testing.T) {
			db := &fakeDBStore{createdRoom: created()}
			h := NewRoomHandler(db)

			rec := serveCreateRoom(t, h, userID, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if db.createRoomCalls != 0 {
				t.Error("invalid request reached DB")
			}
		})
	}

	// 長度用字元數算：30 個中文（90 bytes）要通過
	t.Run("exactly 30 runes is ok", func(t *testing.T) {
		name := strings.Repeat("哈", 30)
		db := &fakeDBStore{createdRoom: created()}
		h := NewRoomHandler(db)

		rec := serveCreateRoom(t, h, userID, roomNameBody(name))

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
		if db.gotRoomName != name {
			t.Errorf("name = %q, want %q", db.gotRoomName, name)
		}
	})

	// 合法名稱必須原樣存進去（不 trim、不 normalize）
	valid := []struct {
		name string
		room string
	}{
		{"full-width punctuation", "好喔！"},
		{"bopomofo and kaomoji", "ㄏㄏ (╯°□°)╯"},
		{"single ASCII space", "房 間"},
		{"single full-width space", "房\u3000間"},
		{"exactly 2 combining marks", "e\u0302\u0301"},
		// 兩組各 2 個記號，中間隔著一般字母：分開計算，所以通過
		{"two groups of 2 marks separated by a letter", "e\u0301\u0301a\u0301\u0301"},
		{"NFD accent stays un-normalized", "cafe\u0301"},
	}
	for _, tc := range valid {
		t.Run(tc.name+" is stored as-is", func(t *testing.T) {
			db := &fakeDBStore{createdRoom: created()}
			h := NewRoomHandler(db)

			rec := serveCreateRoom(t, h, userID, roomNameBody(tc.room))

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
			}
			if db.gotRoomName != tc.room {
				t.Errorf("saved name = %q, want %q", db.gotRoomName, tc.room)
			}
		})
	}

	t.Run("room limit returns 403", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{createRoomErr: model.ErrRoomLimitReached})

		rec := serveCreateRoom(t, h, userID, roomNameBody("房間"))

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if got := decodeErrorBody(t, rec).Code; got != "room_limit_reached" {
			t.Errorf("code = %q, want room_limit_reached", got)
		}
	})

	t.Run("deleted owner returns same 401 as RequireAuth", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{createRoomErr: model.ErrRoomOwnerNotFound})

		rec := serveCreateRoom(t, h, userID, roomNameBody("房間"))

		want := httptest.NewRecorder()
		httpx.WriteUnauthorized(want)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if rec.Body.String() != want.Body.String() {
			t.Errorf("body = %q, want %q", rec.Body.String(), want.Body.String())
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want Bearer", got)
		}
	})

	t.Run("db error returns 500", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{createRoomErr: errors.New("db down")})

		rec := serveCreateRoom(t, h, userID, roomNameBody("房間"))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	// handler 沒經過 RequireAuth（route 註冊錯）：fail closed 回 500，不存任何東西
	t.Run("missing user id in context returns 500", func(t *testing.T) {
		db := &fakeDBStore{}
		h := NewRoomHandler(db)
		req := httptest.NewRequest(http.MethodPost, "/rooms", strings.NewReader(roomNameBody("房間")))
		rec := httptest.NewRecorder()

		h.HandleCreateRoom(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if db.createRoomCalls != 0 {
			t.Error("saved without a user id")
		}
	})
}

// 走 mux：GET /rooms/{public_id} 的 path value 才會被填上
func serveGetRoom(h *RoomHandler, publicID string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rooms/{public_id}", h.HandleGetRoom)
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+publicID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleGetRoom(t *testing.T) {
	t.Run("found returns 200 with room", func(t *testing.T) {
		db := &fakeDBStore{room: model.Room{ID: 5, OwnerID: 7, PublicID: "abcDEF12345", OwnerUsername: "Jay", Name: "房間"}}
		h := NewRoomHandler(db)

		rec := serveGetRoom(h, "abcDEF12345")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if db.gotRoomPublic != "abcDEF12345" {
			t.Errorf("GetRoomByPublicID got %q, want %q", db.gotRoomPublic, "abcDEF12345")
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["public_id"] != "abcDEF12345" || body["room_name"] != "房間" || body["owner_username"] != "Jay" {
			t.Errorf("body = %v, want the room", body)
		}
		if _, ok := body["id"]; ok {
			t.Errorf("body contains internal id: %v", body)
		}
	})

	t.Run("not found returns 404", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{getRoomErr: model.ErrRoomNotFound})

		rec := serveGetRoom(h, "zzzZZZ99999")

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("db error returns 500", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{getRoomErr: errors.New("db down")})

		rec := serveGetRoom(h, "abcDEF12345")

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

// GET /rooms 與 GET /me/rooms 共用的送出 helper
// mine = true 走 /me/rooms（用 RequireAuth 包起來送 Bearer token），否則走公開的 /rooms
func serveListRooms(t *testing.T, h *RoomHandler, mine bool, userID int64, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/rooms"
	handlerFunc := http.HandlerFunc(h.HandleListRooms)
	if mine {
		path = "/me/rooms"
		handlerFunc = http.HandlerFunc(h.HandleListMyRooms)
	}
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	var h2 http.Handler = handlerFunc
	if mine {
		token, _, err := testTokens.IssueAccessToken(userID)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		h2 = middleware.RequireAuth(testTokens)(handlerFunc)
	}
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	return rec
}

func TestHandleListRooms(t *testing.T) {
	const userID = int64(7)
	cursorTime := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	cursor := model.RoomCursor{CreatedAt: cursorTime, PublicID: "abcDEF12340"}

	// 兩個 endpoint 的共同行為都跑一次
	for _, mine := range []bool{false, true} {
		name := "GET /rooms"
		if mine {
			name = "GET /me/rooms"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("limit passes through", func(t *testing.T) {
				cases := []struct {
					query string
					want  int
				}{
					{"", defaultRoomListLimit},
					{"limit=1", 1},
					{"limit=" + strconv.Itoa(model.MaxRoomListLimit), model.MaxRoomListLimit},
				}
				for _, tc := range cases {
					db := &fakeDBStore{}
					rec := serveListRooms(t, NewRoomHandler(db), mine, userID, tc.query)
					if rec.Code != http.StatusOK {
						t.Fatalf("%q: status = %d, want 200 (body: %q)", tc.query, rec.Code, rec.Body.String())
					}
					if db.listRoomsCalls != 1 || db.gotListLimit != tc.want {
						t.Errorf("%q: store calls=%d limit=%d, want 1 call with limit %d", tc.query, db.listRoomsCalls, db.gotListLimit, tc.want)
					}
				}
			})

			t.Run("invalid limit returns 400 without hitting store", func(t *testing.T) {
				for _, v := range []string{"", "abc", "1.5", "0", "-1", strconv.Itoa(model.MaxRoomListLimit + 1)} {
					db := &fakeDBStore{}
					rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "limit="+v)
					if rec.Code != http.StatusBadRequest {
						t.Errorf("limit=%q: status = %d, want 400", v, rec.Code)
					}
					if got := decodeErrorBody(t, rec).Code; got != "invalid_limit" {
						t.Errorf("limit=%q: code = %q, want %q", v, got, "invalid_limit")
					}
					if db.listRoomsCalls != 0 {
						t.Errorf("limit=%q: store called %d times, want 0", v, db.listRoomsCalls)
					}
				}
			})

			t.Run("invalid page_token returns 400 without hitting store", func(t *testing.T) {
				// base64url of `{}`
				for _, v := range []string{"!!!", "e30"} {
					db := &fakeDBStore{}
					rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "page_token="+v)
					if rec.Code != http.StatusBadRequest {
						t.Errorf("page_token=%q: status = %d, want 400", v, rec.Code)
					}
					if got := decodeErrorBody(t, rec).Code; got != "invalid_cursor" {
						t.Errorf("page_token=%q: code = %q, want %q", v, got, "invalid_cursor")
					}
					if db.listRoomsCalls != 0 {
						t.Errorf("page_token=%q: store called %d times, want 0", v, db.listRoomsCalls)
					}
				}
			})

			t.Run("valid page_token reaches store as cursor", func(t *testing.T) {
				token, err := encodePageToken(cursor)
				if err != nil {
					t.Fatalf("encode token: %v", err)
				}
				db := &fakeDBStore{}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "page_token="+token)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body: %q)", rec.Code, rec.Body.String())
				}
				if db.gotListAfter == nil || !db.gotListAfter.CreatedAt.Equal(cursor.CreatedAt) || db.gotListAfter.PublicID != cursor.PublicID {
					t.Errorf("store got after = %+v, want %+v", db.gotListAfter, cursor)
				}
			})

			t.Run("empty page_token is first page", func(t *testing.T) {
				db := &fakeDBStore{}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "page_token=")
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if db.listRoomsCalls != 1 || db.gotListAfter != nil {
					t.Errorf("store calls=%d after=%+v, want 1 call with nil after", db.listRoomsCalls, db.gotListAfter)
				}
			})

			t.Run("next cursor becomes next_page_token", func(t *testing.T) {
				db := &fakeDBStore{
					listRooms:      []model.Room{{ID: 1, OwnerID: 3, PublicID: "abcDEF12345", Name: "房間", CreatedAt: cursorTime}},
					listNextCursor: &cursor,
				}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "")
				var body struct {
					NextPageToken string `json:"next_page_token"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				got, err := decodePageToken(body.NextPageToken)
				if err != nil {
					t.Fatalf("decode next_page_token %q: %v", body.NextPageToken, err)
				}
				if !got.CreatedAt.Equal(cursor.CreatedAt) || got.PublicID != cursor.PublicID {
					t.Errorf("next_page_token decodes to %+v, want %+v", got, cursor)
				}
			})

			t.Run("nil cursor omits next_page_token", func(t *testing.T) {
				db := &fakeDBStore{listRooms: []model.Room{{PublicID: "abcDEF12345", Name: "房間"}}}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "")
				var body map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if _, ok := body["next_page_token"]; ok {
					t.Errorf("body has next_page_token: %s", rec.Body.String())
				}
			})

			t.Run("empty result is [] not null", func(t *testing.T) {
				db := &fakeDBStore{}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if got := strings.TrimSpace(string(body["rooms"])); got != "[]" {
					t.Errorf("rooms = %s, want []", got)
				}
			})

			t.Run("items have only room_name and public_id", func(t *testing.T) {
				db := &fakeDBStore{listRooms: []model.Room{
					{ID: 1, OwnerID: 3, PublicID: "abcDEF12345", Name: "房間一", CreatedAt: cursorTime},
					{ID: 2, OwnerID: 4, PublicID: "ghiJKL56789", Name: "房間二", CreatedAt: cursorTime},
				}}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "")
				var body struct {
					Rooms []map[string]any `json:"rooms"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if len(body.Rooms) != 2 {
					t.Fatalf("len(rooms) = %d, want 2", len(body.Rooms))
				}
				for i, item := range body.Rooms {
					if len(item) != 2 || item["room_name"] == nil || item["public_id"] == nil {
						t.Errorf("rooms[%d] = %v, want exactly room_name and public_id", i, item)
					}
				}
				if body.Rooms[0]["room_name"] != "房間一" || body.Rooms[0]["public_id"] != "abcDEF12345" {
					t.Errorf("rooms[0] = %v, want 房間一 / abcDEF12345", body.Rooms[0])
				}
			})

			t.Run("store error returns 500", func(t *testing.T) {
				db := &fakeDBStore{listRoomsErr: errors.New("boom")}
				rec := serveListRooms(t, NewRoomHandler(db), mine, userID, "")
				if rec.Code != http.StatusInternalServerError {
					t.Errorf("status = %d, want 500", rec.Code)
				}
				if strings.Contains(rec.Body.String(), "boom") {
					t.Errorf("body leaks error: %q", rec.Body.String())
				}
			})
		})
	}

	t.Run("GET /rooms passes nil userID", func(t *testing.T) {
		db := &fakeDBStore{}
		serveListRooms(t, NewRoomHandler(db), false, 0, "")
		if db.listRoomsCalls != 1 || db.gotListUserID != nil {
			t.Errorf("calls=%d userID=%v, want 1 call with nil userID", db.listRoomsCalls, db.gotListUserID)
		}
	})

	t.Run("GET /me/rooms passes caller's userID", func(t *testing.T) {
		db := &fakeDBStore{}
		serveListRooms(t, NewRoomHandler(db), true, userID, "")
		if db.listRoomsCalls != 1 || db.gotListUserID == nil || *db.gotListUserID != userID {
			t.Errorf("calls=%d userID=%v, want 1 call with userID %d", db.listRoomsCalls, db.gotListUserID, userID)
		}
	})

	t.Run("GET /me/rooms sets Cache-Control no-store", func(t *testing.T) {
		db := &fakeDBStore{}
		rec := serveListRooms(t, NewRoomHandler(db), true, userID, "")
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		// 錯誤回應也要有
		rec = serveListRooms(t, NewRoomHandler(db), true, userID, "limit=abc")
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("on 400, Cache-Control = %q, want no-store", got)
		}
	})
}
