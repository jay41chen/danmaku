package handler

import (
	"context"
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
)

// 用 RequireAuth 包起來送 Bearer token，user id 才會進到 ctx（context key 沒有也不該 export setter）
// RequireAuth 還是 stub 時會 panic：recover 成 test 失敗，不然整個 package 的 test 都會被中斷
// 打 POST /rooms/{public_id}/messages
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

// 以下測的是共用的 createMessage 行為（驗證 / 限流 / 發佈），透過房間 handler 呼叫
func TestCreateMessage(t *testing.T) {
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

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("哈哈"))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if db.gotMsgRoomPublic != model.LegacyRoomPublicID {
			t.Errorf("CreateMessage room = %q, want legacy %q", db.gotMsgRoomPublic, model.LegacyRoomPublicID)
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
		if redis.publishedRooms[0] != model.LegacyRoomPublicID {
			t.Errorf("published room = %q, want %q", redis.publishedRooms[0], model.LegacyRoomPublicID)
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

	// INSERT 成功後 client 斷線（request ctx 被 cancel）：已存 DB 的訊息仍要廣播
	// 廣播收到的 ctx 必須：沒被取消、有 publishTimeout 內的 deadline、保留 request ctx 的 value
	t.Run("broadcast survives client disconnect after insert", func(t *testing.T) {
		type ctxKey struct{}
		base, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-123"))
		defer cancel()
		db := &fakeDBStore{createdMsg: created(), onCreateMessage: cancel}
		redis := &fakeRedis{}
		h := NewMessageHandler(db, redis, &fakeRateLimiter{})
		publicID := model.LegacyRoomPublicID
		req := httptest.NewRequest(http.MethodPost, "/rooms/"+publicID+"/messages", strings.NewReader(contentBody("哈哈"))).WithContext(base)
		req.SetPathValue("public_id", publicID)

		start := time.Now()
		rec := serveAuthed(t, http.HandlerFunc(h.HandleCreateRoomMessage), userID, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if len(redis.publishCtxs) != 1 {
			t.Fatalf("Publish called %d times, want 1", len(redis.publishCtxs))
		}
		pctx := redis.publishCtxs[0]
		if err := pctx.err; err != nil {
			t.Errorf("publish ctx Err() = %v, want nil (should not follow request cancel)", err)
		}
		deadline, ok := pctx.deadline, pctx.hasDeadline
		if !ok {
			t.Fatalf("publish ctx has no deadline, want one within %v", publishTimeout)
		}
		if deadline.After(start.Add(publishTimeout + 100*time.Millisecond)) {
			t.Errorf("publish deadline = %v after start, want <= ~%v", deadline.Sub(start), publishTimeout)
		}
		if got := pctx.ctx.Value(ctxKey{}); got != "req-123" {
			t.Errorf("publish ctx value = %v, want req-123 (WithoutCancel should keep values)", got)
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
		// precis Freeform 擋掉的字元
		{"zero-width space only", contentBody("\u200b")},
		{"zero-width space inside text", contentBody("哈\u200b哈")},
		{"right-to-left override", contentBody("abc\u202eevil")},
		{"newline in middle", contentBody("a\nb")},
		{"tab in middle", contentBody("a\tb")},
		{"ZWJ family emoji", contentBody("\U0001F468\u200d\U0001F469\u200d\U0001F467")},
	}
	for _, tc := range invalid {
		t.Run(tc.name+" returns 400", func(t *testing.T) {
			db := &fakeDBStore{createdMsg: created()}
			redis := &fakeRedis{}
			h := NewMessageHandler(db, redis, &fakeRateLimiter{})

			rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if db.createMsgCalled || len(redis.published) != 0 {
				t.Error("invalid request reached DB or Redis")
			}
		})
	}

	// 合法內容必須原樣存進去（byte-for-byte，不 normalize）
	// 這張表是防止規則寫太嚴（例如改成白名單）而誤殺正常訊息
	valid := []struct {
		name    string
		content string
	}{
		{"full-width punctuation", "好喔！"},
		{"bopomofo", "ㄏㄏ"},
		{"kaomoji", "(╯°□°)╯"},
		{"emoji", "\U0001F602"},
		{"NFD accent stays un-normalized", "cafe\u0301"},
		// Zalgo 訊息允許，渲染交給前端處理
		{"zalgo combining marks", "Z\u0301\u0301\u0301\u0301"},
	}
	for _, tc := range valid {
		t.Run(tc.name+" is stored as-is", func(t *testing.T) {
			db := &fakeDBStore{createdMsg: created()}
			h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

			rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody(tc.content))

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
			}
			if db.gotMsgContent != tc.content {
				t.Errorf("saved content = %q, want %q", db.gotMsgContent, tc.content)
			}
		})
	}

	// 長度用字元數算：100 個中文（300 bytes）要通過
	t.Run("exactly 100 runes is ok", func(t *testing.T) {
		content := strings.Repeat("哈", 100)
		db := &fakeDBStore{createdMsg: created()}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody(content))

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

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("哈 "))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
		if db.gotMsgContent != "哈 " {
			t.Errorf("saved content = %q, want %q", db.gotMsgContent, "哈 ")
		}
	})

	t.Run("deleted user returns same 401 as RequireAuth", func(t *testing.T) {
		db := &fakeDBStore{createMsgErr: model.ErrMessageUserNotFound}
		redis := &fakeRedis{}
		h := NewMessageHandler(db, redis, &fakeRateLimiter{})

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("hi"))

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

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("hi"))

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

		rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("hi"))

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
	})

	// handler 沒經過 RequireAuth（route 註冊錯）：fail closed 回 500，不存任何東西
	t.Run("missing user id in context returns 500", func(t *testing.T) {
		db := &fakeDBStore{}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})
		req := httptest.NewRequest(http.MethodPost, "/rooms/abc/messages", strings.NewReader(contentBody("hi")))
		req.SetPathValue("public_id", "abc")
		rec := httptest.NewRecorder()

		h.HandleCreateRoomMessage(rec, req)

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
	// 沒有發送者的訊息：user_id / username 要序列化成 null，不是省略也不是空字串
	t.Run("user fields serialize as id/username or null", func(t *testing.T) {
		uid, name := int64(7), "Jay"
		h := NewMessageHandler(&fakeDBStore{msgs: []model.Message{
			{ID: 2, UserID: &uid, Username: &name, Content: "hi"},
			{ID: 1, Content: "old"},
		}}, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "abc")

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
	t.Run("room not found returns 404", func(t *testing.T) {
		db := &fakeDBStore{getRoomErr: model.ErrRoomNotFound}
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
	t.Run("query messages error returns 500", func(t *testing.T) {
		db := &fakeDBStore{room: model.Room{ID: 9}, err: errors.New("db down")}
		h := NewMessageHandler(db, &fakeRedis{}, &fakeRateLimiter{})

		rec := serve(h, "abc")

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
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
		h := NewMessageHandler(&fakeDBStore{createMsgErr: model.ErrRoomNotFound}, redis, &fakeRateLimiter{})

		rec := serveCreateRoomMessage(t, h, userID, "nope", contentBody("hi"))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		if len(redis.published) != 0 {
			t.Error("published despite failed save")
		}
	})
}

func TestCreateMessageRateLimit(t *testing.T) {
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

			rec := serveCreateRoomMessage(t, h, userID, model.LegacyRoomPublicID, contentBody("hi"))

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
