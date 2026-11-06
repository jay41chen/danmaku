package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 這個檔案鎖住「錯誤回應的資安規則」：
// 這些情況刻意不拆成不同 code，拆開就等於告訴攻擊者多一點資訊。
// 每個 table 都用「整個 body 逐字相等」比對，不只比 code，這樣 message 被偷偷改掉也抓得到

const (
	bodyUnauthorized       = "{\"error\":{\"code\":\"unauthorized\",\"message\":\"unauthorized\"}}\n"
	bodyInvalidCredentials = "{\"error\":{\"code\":\"invalid_credentials\",\"message\":\"invalid username or password\"}}\n"
	bodyInvalidRefresh     = "{\"error\":{\"code\":\"invalid_refresh_token\",\"message\":\"invalid refresh token\"}}\n"
	bodyRoomNotFound       = "{\"error\":{\"code\":\"room_not_found\",\"message\":\"room not found\"}}\n"
	bodyInternalError      = "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal server error\"}}\n"
)

// 登入：帳號不存在、密碼錯、帳號格式不合法，三者必須完全一樣（也不能從 timing 以外的地方分辨）
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	password := "kq7vz2mx"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	stored := model.User{ID: 7, Username: "Jay", PasswordHash: hash}

	cases := []struct {
		name   string
		body   string
		user   model.User
		getErr error
	}{
		{"unknown account", `{"username":"nobody","password":"kq7vz2mx"}`, model.User{}, model.ErrUserNotFound},
		{"wrong password", `{"username":"jay","password":"wrong-password"}`, stored, nil},
		{"malformed username", `{"username":"bad name!","password":"kq7vz2mx"}`, stored, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(&fakeDBStore{user: tc.user, getErr: tc.getErr}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()

			h.HandleLogin(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(tc.body)))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if got := rec.Body.String(); got != bodyInvalidCredentials {
				t.Errorf("body = %q, want %q", got, bodyInvalidCredentials)
			}
		})
	}
}

// 所有「沒登入」的 401：RequireAuth 的各種失敗，加上「token 有效但 user 已刪除」的各個 handler，body 和 header 完全一樣
func TestUnauthorizedResponsesAreIdentical(t *testing.T) {
	const userID = int64(7)
	validToken, _, err := testTokens.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	expiredIssuer, err := auth.NewTokenIssuer(strings.Repeat("k", 32), -time.Hour)
	if err != nil {
		t.Fatalf("expired issuer: %v", err)
	}
	expiredToken, _, err := expiredIssuer.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("next called, want blocked") })
	rejects := []struct{ name, header string }{
		{"missing header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bad token", "Bearer not-a-jwt"},
		{"expired token", "Bearer " + expiredToken},
	}

	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{}
	for _, rj := range rejects {
		cases = append(cases, struct {
			name string
			do   func() *httptest.ResponseRecorder
		}{"RequireAuth " + rj.name, func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			if rj.header != "" {
				req.Header.Set("Authorization", rj.header)
			}
			rec := httptest.NewRecorder()
			middleware.RequireAuth(testTokens)(next).ServeHTTP(rec, req)
			return rec
		}})
	}
	authed := func(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("Authorization", "Bearer "+validToken)
		rec := httptest.NewRecorder()
		middleware.RequireAuth(testTokens)(h).ServeHTTP(rec, req)
		return rec
	}
	cases = append(cases, []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"valid token, user deleted: GET /me", func() *httptest.ResponseRecorder {
			h := NewAuthHandler(&fakeDBStore{getByIDErr: model.ErrUserNotFound}, &fakeRateLimiter{}, "", testTokens)
			return authed(http.HandlerFunc(h.HandleMe), httptest.NewRequest(http.MethodGet, "/me", nil))
		}},
		{"valid token, user deleted: POST /rooms", func() *httptest.ResponseRecorder {
			h := NewRoomHandler(&fakeDBStore{createRoomErr: model.ErrRoomOwnerNotFound})
			return authed(http.HandlerFunc(h.HandleCreateRoom), httptest.NewRequest(http.MethodPost, "/rooms", strings.NewReader(roomNameBody("room"))))
		}},
		{"valid token, user deleted: POST message", func() *httptest.ResponseRecorder {
			h := NewMessageHandler(&fakeDBStore{createMsgErr: model.ErrMessageUserNotFound}, &fakeRedis{}, &fakeRateLimiter{})
			req := httptest.NewRequest(http.MethodPost, "/rooms/x/messages", strings.NewReader(contentBody("hi")))
			req.SetPathValue("public_id", model.LegacyRoomPublicID)
			return authed(http.HandlerFunc(h.HandleCreateRoomMessage), req)
		}},
	}...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do()

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got := rec.Body.String(); got != bodyUnauthorized {
				t.Errorf("body = %q, want %q", got, bodyUnauthorized)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
		})
	}
}

// 房間不存在、已 soft delete：handler 看到的都是 ErrRoomNotFound（store 負責把兩者合併），所有出口都回同一份 404
func TestRoomNotFoundResponsesAreIdentical(t *testing.T) {
	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"GET room", func() *httptest.ResponseRecorder {
			return serveGetRoom(NewRoomHandler(&fakeDBStore{getRoomErr: model.ErrRoomNotFound}), "missing")
		}},
		{"GET messages", func() *httptest.ResponseRecorder {
			h := NewMessageHandler(&fakeDBStore{getRoomErr: model.ErrRoomNotFound}, &fakeRedis{}, &fakeRateLimiter{})
			req := httptest.NewRequest(http.MethodGet, "/rooms/missing/messages", nil)
			req.SetPathValue("public_id", "missing")
			rec := httptest.NewRecorder()
			h.HandleGetRoomMessages(rec, req)
			return rec
		}},
		{"POST message", func() *httptest.ResponseRecorder {
			h := NewMessageHandler(&fakeDBStore{createMsgErr: model.ErrRoomNotFound}, &fakeRedis{}, &fakeRateLimiter{})
			return serveCreateRoomMessage(t, h, 7, "missing", contentBody("hi"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do()

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if got := rec.Body.String(); got != bodyRoomNotFound {
				t.Errorf("body = %q, want %q", got, bodyRoomNotFound)
			}
		})
	}
}

// refresh token 不管是不存在、過期、被撤銷、重複使用，都是同一份 401
func TestInvalidRefreshTokenResponsesAreIdentical(t *testing.T) {
	cases := []struct {
		name      string
		rotateErr error
	}{
		{"unknown or expired", model.ErrRefreshTokenInvalid},
		{"duplicate use", &model.RefreshTokenDuplicateUseError{UserID: 7}},
		{"issued before revoke", &model.RefreshTokenRevokedError{UserID: 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(&fakeDBStore{rotateErr: tc.rotateErr}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()

			h.HandleRefresh(rec, httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(`{"refresh_token":"x"}`)))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got := rec.Body.String(); got != bodyInvalidRefresh {
				t.Errorf("body = %q, want %q", got, bodyInvalidRefresh)
			}
		})
	}
}

// 429：code 是 rate_limited，而且 Retry-After 一定要在（少了 client 不知道要等多久）
func TestRateLimitedResponse(t *testing.T) {
	limiter := &fakeRateLimiter{blockKey: "rl:login:ip:192.0.2.1", retryAfter: 30 * time.Second}
	h := NewAuthHandler(&fakeDBStore{}, limiter, "", testTokens)
	rec := httptest.NewRecorder()

	h.HandleLogin(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"jay","password":"kq7vz2mx"}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Result().Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := decodeErrorBody(t, rec).Code; got != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", got)
	}
}

// 500 不能帶任何內部錯誤文字：store 回的 error 內容只進 log
// 每個會碰 DB 的 handler 都要驗：任何一個把 err.Error() 寫進 body 都會被抓到
func TestInternalErrorDoesNotLeak(t *testing.T) {
	const secret = "pq: password authentication failed for user danmaku"
	leak := errors.New(secret)

	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"login", func() *httptest.ResponseRecorder {
			h := NewAuthHandler(&fakeDBStore{getErr: leak}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()
			h.HandleLogin(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"jay","password":"kq7vz2mx"}`)))
			return rec
		}},
		{"register", func() *httptest.ResponseRecorder {
			h := NewAuthHandler(&fakeDBStore{createErr: leak}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()
			h.HandleRegister(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"username":"jay","password":"kq7vz2mx"}`)))
			return rec
		}},
		{"refresh rotate", func() *httptest.ResponseRecorder {
			h := NewAuthHandler(&fakeDBStore{rotateErr: leak}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()
			h.HandleRefresh(rec, httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(`{"refresh_token":"x"}`)))
			return rec
		}},
		{"createMessage", func() *httptest.ResponseRecorder {
			h := NewMessageHandler(&fakeDBStore{createMsgErr: leak}, &fakeRedis{}, &fakeRateLimiter{})
			return serveCreateRoomMessage(t, h, 7, "abcDEF12345", contentBody("hi"))
		}},
		{"createRoom", func() *httptest.ResponseRecorder {
			h := NewRoomHandler(&fakeDBStore{createRoomErr: leak})
			return serveCreateRoom(t, h, 7, roomNameBody("room"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do()

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("body leaks internal error: %q", rec.Body.String())
			}
			if got := decodeErrorBody(t, rec).Code; got != "internal_error" {
				t.Errorf("code = %q, want internal_error", got)
			}
			if got := rec.Body.String(); got != bodyInternalError {
				t.Errorf("body = %q, want %q", got, bodyInternalError)
			}
		})
	}
}

// 壞掉的 JSON body 一律是 invalid_json：code 是 API 合約，client 靠它顯示文字
func TestInvalidJSONCode(t *testing.T) {
	const broken = `{"x":`
	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"createRoom", func() *httptest.ResponseRecorder {
			return serveCreateRoom(t, NewRoomHandler(&fakeDBStore{}), 7, broken)
		}},
		{"createMessage", func() *httptest.ResponseRecorder {
			h := NewMessageHandler(&fakeDBStore{}, &fakeRedis{}, &fakeRateLimiter{})
			return serveCreateRoomMessage(t, h, 7, "abcDEF12345", broken)
		}},
		{"logout", func() *httptest.ResponseRecorder {
			h := NewAuthHandler(&fakeDBStore{}, &fakeRateLimiter{}, "", testTokens)
			rec := httptest.NewRecorder()
			h.HandleLogout(rec, httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(broken)))
			return rec
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do()

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if got := decodeErrorBody(t, rec).Code; got != "invalid_json" {
				t.Errorf("code = %q, want invalid_json", got)
			}
		})
	}
}

// 每個驗證 sentinel 都要帶預期的 code：code 是 API 合約，改名會被這裡抓到
func TestValidationSentinelCodes(t *testing.T) {
	cases := []struct {
		err  *model.CodedError
		want string
	}{
		{errContentEmpty, "content_empty"},
		{errContentTooLong, "content_too_long"},
		{errContentInvalid, "content_invalid"},
		{errRoomNameInvalid, "room_name_invalid"},
		{errRoomNameLength, "room_name_length"},
		{errRoomNameSurroundingSpace, "room_name_surrounding_space"},
		{errRoomNameConsecutiveSpaces, "room_name_consecutive_spaces"},
		{errRoomNameTooManyMarks, "room_name_too_many_marks"},
		{errInvalidPageToken, "invalid_cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if tc.err.Code != tc.want {
				t.Errorf("code = %q, want %q", tc.err.Code, tc.want)
			}
			if tc.err.Message == "" {
				t.Error("message is empty")
			}
		})
	}

	// 從 validator 實際回傳的值驗：確認 validate 函式回的是這些 sentinel，不是別的
	validators := []struct {
		name string
		got  error
		want string
	}{
		{"empty content", validateMessageContent("   "), "content_empty"},
		{"long content", validateMessageContent(strings.Repeat("a", maxMessageRunes+1)), "content_too_long"},
		{"invalid content", validateMessageContent("a\x00b"), "content_invalid"},
		{"invalid room name", validateRoomName("a\x00b"), "room_name_invalid"},
		{"room name length", validateRoomName(""), "room_name_length"},
		{"surrounding space", validateRoomName(" a"), "room_name_surrounding_space"},
		{"consecutive spaces", validateRoomName("a  b"), "room_name_consecutive_spaces"},
		{"too many marks", validateRoomName("á́́"), "room_name_too_many_marks"},
	}
	for _, tc := range validators {
		t.Run("validator "+tc.name, func(t *testing.T) {
			var coded *model.CodedError
			if !errors.As(tc.got, &coded) || coded.Code != tc.want {
				t.Errorf("err = %v, want CodedError with code %q", tc.got, tc.want)
			}
		})
	}
	if _, err := decodePageToken("!!!"); !errors.Is(err, errInvalidPageToken) {
		t.Errorf("decodePageToken err = %v, want errInvalidPageToken", err)
	}
}
