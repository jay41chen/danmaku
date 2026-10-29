package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// 同 serveCreateMessage：用 RequireAuth 包起來送 Bearer token，user id 才會進到 ctx
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
		return model.Room{ID: 5, OwnerID: userID, PublicID: "abcDEF1234", OwnerUsername: "Jay", Name: "我的房間",
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
		if body["public_id"] != "abcDEF1234" || body["room_name"] != "我的房間" || body["owner_username"] != "Jay" {
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

	// 跟訊息不同：存的是 trim 後的名稱；長度也是 trim 後才算
	t.Run("name is trimmed before saving", func(t *testing.T) {
		db := &fakeDBStore{createdRoom: created()}
		h := NewRoomHandler(db)

		rec := serveCreateRoom(t, h, userID, roomNameBody("  房間 \n"))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
		if db.gotRoomName != "房間" {
			t.Errorf("saved name = %q, want %q", db.gotRoomName, "房間")
		}
	})

	t.Run("room limit returns 403", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{createRoomErr: store.ErrRoomLimitReached})

		rec := serveCreateRoom(t, h, userID, roomNameBody("房間"))

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("deleted owner returns same 401 as RequireAuth", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{createRoomErr: store.ErrRoomOwnerNotFound})

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
		db := &fakeDBStore{room: model.Room{ID: 5, OwnerID: 7, PublicID: "abcDEF1234", OwnerUsername: "Jay", Name: "房間"}}
		h := NewRoomHandler(db)

		rec := serveGetRoom(h, "abcDEF1234")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if db.gotRoomPublic != "abcDEF1234" {
			t.Errorf("GetRoomByPublicID got %q, want %q", db.gotRoomPublic, "abcDEF1234")
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["public_id"] != "abcDEF1234" || body["room_name"] != "房間" || body["owner_username"] != "Jay" {
			t.Errorf("body = %v, want the room", body)
		}
		if _, ok := body["id"]; ok {
			t.Errorf("body contains internal id: %v", body)
		}
	})

	t.Run("not found returns 404", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{getRoomErr: store.ErrRoomNotFound})

		rec := serveGetRoom(h, "nope")

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("db error returns 500", func(t *testing.T) {
		h := NewRoomHandler(&fakeDBStore{getRoomErr: errors.New("db down")})

		rec := serveGetRoom(h, "abcDEF1234")

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}
