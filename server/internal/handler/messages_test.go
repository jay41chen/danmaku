package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

func TestHandleGetMessages(t *testing.T) {
	t.Run("db error returns 500", func(t *testing.T) {
		h := NewMessageHandler(&fakeDBStore{err: errors.New("db down")}, &fakeRedis{}, &fakeRateLimiter{})
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleGetMessages(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
	t.Run("returns oldest first", func(t *testing.T) {
		input := []model.Message{{ID: 3, Content: "c"},
			{ID: 2, Content: "b"},
			{ID: 1, Content: "a"}}
		// copy一份input並翻轉，這是我們期待的結果
		expect := slices.Clone(input)
		slices.Reverse(expect)
		db := &fakeDBStore{msgs: input, room: model.Room{ID: 1, PublicID: store.LegacyRoomPublicID}}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleGetMessages(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		// 已棄用的 /messages 固定走 legacy 房間
		if db.gotRoomPublic != store.LegacyRoomPublicID || db.gotQueryRoomID != 1 {
			t.Errorf("got room (%q, %d), want (%q, 1)", db.gotRoomPublic, db.gotQueryRoomID, store.LegacyRoomPublicID)
		}

		var got []model.Message
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}

		if !slices.EqualFunc(expect, got, func(a, b model.Message) bool { return a.ID == b.ID && a.Content == b.Content }) {
			t.Errorf("expect = %+v, got %+v", expect, got)
		}
	})
	// 沒有發送者的訊息：user_id / username 要序列化成 null，不是省略也不是空字串
	t.Run("user fields serialize as id/username or null", func(t *testing.T) {
		uid, name := int64(7), "Jay"
		h := NewMessageHandler(&fakeDBStore{msgs: []model.Message{
			{ID: 2, UserID: &uid, Username: &name, Content: "hi"},
			{ID: 1, Content: "old"},
		}}, &fakeRedis{}, &fakeRateLimiter{})
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleGetMessages(rec, req)

		var got []map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// 反轉後：舊的在前
		if v, ok := got[0]["user_id"]; !ok || v != nil {
			t.Errorf("old message user_id = %v (present=%v), want null", v, ok)
		}
		if v, ok := got[0]["username"]; !ok || v != nil {
			t.Errorf("old message username = %v (present=%v), want null", v, ok)
		}
		if got[1]["user_id"] != float64(7) || got[1]["username"] != "Jay" {
			t.Errorf("message = %v, want user_id 7 / username Jay", got[1])
		}
	})
}

// 用 RequireAuth 包起來送 Bearer token，user id 才會進到 ctx（context key 沒有也不該 export setter）
// RequireAuth 還是 stub 時會 panic：recover 成 test 失敗，不然整個 package 的 test 都會被中斷
func serveCreateMessage(t *testing.T, h *MessageHandler, userID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body))
	return serveAuthed(t, http.HandlerFunc(h.HandleCreateMessage), userID, req)
}

// 同上，但打 POST /rooms/{public_id}/messages
func serveCreateRoomMessage(t *testing.T, h *MessageHandler, userID int64, publicID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+publicID+"/messages", strings.NewReader(body))
	req.SetPathValue("public_id", publicID)
	return serveAuthed(t, http.HandlerFunc(h.HandleCreateRoomMessage), userID, req)
}

func serveAuthed(t *testing.T, handler http.Handler, userID int64, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequireAuth panicked (not implemented yet?): %v", r)
		}
	}()
	token, _, err := testTokens.IssueAccessToken(userID)
	if err != nil {
		panic(err)
	}
	wrapped := middleware.RequireAuth(testTokens)(handler)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

func contentBody(content string) string {
	b, _ := json.Marshal(map[string]string{"content": content})
	return string(b)
}

func TestHandleCreateMessage(t *testing.T) {
	const userID = int64(7)
	created := func() model.Message {
		uid, name := userID, "Jay"
		return model.Message{ID: 42, UserID: &uid, Username: &name, Content: "哈哈",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	}

	t.Run("success returns 201 with message and publishes envelope", func(t *testing.T) {
		db := &fakeDBStore{createdMsg: created()}
		redis := &fakeRedis{}
		h := NewMessageHandler(db, redis, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody("哈哈"))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if db.gotMsgRoomPublic != store.LegacyRoomPublicID {
			t.Errorf("CreateMessage room = %q, want legacy %q", db.gotMsgRoomPublic, store.LegacyRoomPublicID)
		}
		if db.gotMsgUserID != userID || db.gotMsgContent != "哈哈" {
			t.Errorf("CreateMessage got (%d, %q), want (%d, %q)", db.gotMsgUserID, db.gotMsgContent, userID, "哈哈")
		}
		// response body 是 message 本身
		var body model.Message
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.ID != 42 || body.Content != "哈哈" || body.Username == nil || *body.Username != "Jay" {
			t.Errorf("body = %+v, want the created message", body)
		}
		// Redis 上的是 envelope
		if len(redis.published) != 1 {
			t.Fatalf("published %d messages, want 1", len(redis.published))
		}
		var env struct {
			Type    string        `json:"type"`
			Payload model.Message `json:"payload"`
		}
		if err := json.Unmarshal([]byte(redis.published[0]), &env); err != nil {
			t.Fatalf("decode published: %v", err)
		}
		if env.Type != wsTypeMessage || env.Payload.ID != 42 || env.Payload.Content != "哈哈" {
			t.Errorf("published = %+v, want message envelope with the created message", env)
		}
	})

	// 驗證失敗的 case：fake DB 不會被呼叫到
	invalid := []struct {
		name string
		body string
	}{
		{"broken json", `{"content":`},
		{"empty content", contentBody("")},
		{"missing content", `{}`},
		{"whitespace only", contentBody("   \t\n")},
		{"full-width space only", contentBody("　　")},
		{"101 runes", contentBody(strings.Repeat("哈", 101))},
	}
	for _, tc := range invalid {
		t.Run(tc.name+" returns 400", func(t *testing.T) {
			db := &fakeDBStore{createdMsg: created()}
			redis := &fakeRedis{}
			h := NewMessageHandler(db, redis, &fakeRateLimiter{})

			rec := serveCreateMessage(t, h, userID, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if db.createMsgCalled || len(redis.published) != 0 {
				t.Error("invalid request reached DB or Redis")
			}
		})
	}

	// 長度用字元數算：100 個中文（300 bytes）要通過
	t.Run("exactly 100 runes is ok", func(t *testing.T) {
		content := strings.Repeat("哈", 100)
		db := &fakeDBStore{createdMsg: created()}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody(content))

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
		if db.gotMsgContent != content {
			t.Error("content was altered before saving")
		}
	})

	// 判斷空白用 trim，但存進去的是原樣
	t.Run("stores content untrimmed", func(t *testing.T) {
		db := &fakeDBStore{createdMsg: created()}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody("哈 "))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
		if db.gotMsgContent != "哈 " {
			t.Errorf("saved content = %q, want %q", db.gotMsgContent, "哈 ")
		}
	})

	t.Run("deleted user returns same 401 as RequireAuth", func(t *testing.T) {
		db := &fakeDBStore{createMsgErr: store.ErrMessageUserNotFound}
		redis := &fakeRedis{}
		h := NewMessageHandler(db, redis, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody("hi"))

		// 期望值直接用 middleware 的 helper 產生：兩邊永遠是同一份
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
		if len(redis.published) != 0 {
			t.Error("published despite failed save")
		}
	})

	t.Run("db error returns 500", func(t *testing.T) {
		redis := &fakeRedis{}
		h := NewMessageHandler(&fakeDBStore{createMsgErr: errors.New("db down")}, redis, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody("hi"))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if len(redis.published) != 0 {
			t.Error("published despite failed save")
		}
	})

	// 已經存進 DB 了，廣播失敗不能讓 client 以為沒送出去（重送會變重複訊息）
	t.Run("publish error still returns 201", func(t *testing.T) {
		h := NewMessageHandler(&fakeDBStore{createdMsg: created()}, &fakeRedis{publishErr: errors.New("redis down")}, &fakeRateLimiter{})

		rec := serveCreateMessage(t, h, userID, contentBody("hi"))

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
	})

	// handler 沒經過 RequireAuth（route 註冊錯）：fail closed 回 500，不存任何東西
	t.Run("missing user id in context returns 500", func(t *testing.T) {
		db := &fakeDBStore{}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})
		req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(contentBody("hi")))
		rec := httptest.NewRecorder()

		h.HandleCreateMessage(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if db.createMsgCalled {
			t.Error("saved without a user id")
		}
	})
}

func TestHandleGetRoomMessages(t *testing.T) {
	serve := func(h *MessageHandler, publicID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/rooms/"+publicID+"/messages", nil)
		req.SetPathValue("public_id", publicID)
		rec := httptest.NewRecorder()
		h.HandleGetRoomMessages(rec, req)
		return rec
	}

	t.Run("uses path public id and room id", func(t *testing.T) {
		db := &fakeDBStore{
			room: model.Room{ID: 9, PublicID: "abc"},
			msgs: []model.Message{{ID: 2, Content: "b"}, {ID: 1, Content: "a"}},
		}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "abc")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if db.gotRoomPublic != "abc" || db.gotQueryRoomID != 9 {
			t.Errorf("got room (%q, %d), want (abc, 9)", db.gotRoomPublic, db.gotQueryRoomID)
		}
		var got []model.Message
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
			t.Errorf("got %+v, want oldest first [1 2]", got)
		}
	})
	t.Run("room not found returns 404", func(t *testing.T) {
		db := &fakeDBStore{getRoomErr: store.ErrRoomNotFound}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "nope")

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		// 查不到房間就不該再查訊息
		if db.queryMsgCalled {
			t.Errorf("QueryMessage called with room id %d, want not called", db.gotQueryRoomID)
		}
	})
	t.Run("other room lookup error returns 500", func(t *testing.T) {
		db := &fakeDBStore{getRoomErr: errors.New("db down")}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "abc")

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		// 查房間失敗就不該再查訊息
		if db.queryMsgCalled {
			t.Errorf("QueryMessage called with room id %d, want not called", db.gotQueryRoomID)
		}
	})
	// 空房間：非 nil 的空 slice 會編成 []；不是 null（client 不用特別處理）
	t.Run("empty room returns empty array", func(t *testing.T) {
		db := &fakeDBStore{room: model.Room{ID: 9}, msgs: []model.Message{}}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "abc")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
			t.Errorf("body = %q, want []", got)
		}
	})
}

func TestHandleCreateRoomMessage(t *testing.T) {
	const userID = int64(7)

	t.Run("success passes path public id and returns 201", func(t *testing.T) {
		db := &fakeDBStore{createdMsg: model.Message{ID: 42, Content: "hi"}}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serveCreateRoomMessage(t, h, userID, "abc", contentBody("hi"))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if db.gotMsgRoomPublic != "abc" || db.gotMsgUserID != userID {
			t.Errorf("CreateMessage got (%q, %d), want (abc, %d)", db.gotMsgRoomPublic, db.gotMsgUserID, userID)
		}
	})
	t.Run("room not found returns 404 and does not publish", func(t *testing.T) {
		redis := &fakeRedis{}
		h := NewMessageHandler(&fakeDBStore{createMsgErr: store.ErrRoomNotFound}, redis, &fakeRateLimiter{})

		rec := serveCreateRoomMessage(t, h, userID, "nope", contentBody("hi"))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		if len(redis.published) != 0 {
			t.Error("published despite failed save")
		}
	})
}

func TestHandleCreateMessageRateLimit(t *testing.T) {
	const userID = int64(7)
	burstKey := messageBurstRule.key(strconv.FormatInt(userID, 10))
	sustainKey := messageSustainRule.key(strconv.FormatInt(userID, 10))

	cases := []struct {
		name           string
		limiter        *fakeRateLimiter
		wantStatus     int
		wantRetryAfter string
		wantSaved      bool
	}{
		{"burst blocked", &fakeRateLimiter{blockKey: burstKey, retryAfter: time.Second}, http.StatusTooManyRequests, "1", false},
		{"sustain blocked", &fakeRateLimiter{blockKey: sustainKey, retryAfter: 8 * time.Second}, http.StatusTooManyRequests, "8", false},
		// 兩條都擋：Retry-After 取比較長的
		{"both blocked uses longer retry after", &fakeRateLimiter{blocks: map[string]time.Duration{burstKey: time.Second, sustainKey: 8 * time.Second}}, http.StatusTooManyRequests, "8", false},
		// limiter 壞掉 fail open：照常存
		{"limiter error fails open", &fakeRateLimiter{err: errors.New("redis down")}, http.StatusCreated, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDBStore{}
			h := NewMessageHandler(db, &fakeRedis{}, tc.limiter)

			rec := serveCreateMessage(t, h, userID, contentBody("hi"))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
			if db.createMsgCalled != tc.wantSaved {
				t.Errorf("saved = %v, want %v", db.createMsgCalled, tc.wantSaved)
			}
			// 兩條規則都要問過，key 是 user id
			if !slices.Equal(tc.limiter.allowKeys, []string{burstKey, sustainKey}) {
				t.Errorf("allow keys = %v, want [%s %s]", tc.limiter.allowKeys, burstKey, sustainKey)
			}
		})
	}
}
