package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

func TestHandleRegister(t *testing.T) {
	const validBody = `{"username":"jay","password":"kq7vz2mx"}`

	// 失敗的 case：形狀都一樣，只差 method / body / fake 回什麼 / 預期 status
	// 驗證失敗的 case 讓 fake 回 ErrUsernameTaken：萬一驗證被拿掉、走到 CreateUser，會變成 409 而不是 400
	cases := []struct {
		name       string
		method     string
		body       string
		createErr  error
		wantStatus int
	}{
		{"broken json", http.MethodPost, `{"username":"jay",}`, store.ErrUsernameTaken, http.StatusBadRequest},
		{"invalid username", http.MethodPost, `{"username":"a","password":"kq7vz2mx"}`, store.ErrUsernameTaken, http.StatusBadRequest},
		{"too short password", http.MethodPost, `{"username":"jay","password":"jayjay"}`, store.ErrUsernameTaken, http.StatusBadRequest},
		{"common password", http.MethodPost, `{"username":"jay","password":"12345678"}`, store.ErrUsernameTaken, http.StatusBadRequest},
		// 以下 body 要完全合法，才走得到 CreateUser
		{"username taken", http.MethodPost, validBody, store.ErrUsernameTaken, http.StatusConflict},
		{"db error", http.MethodPost, validBody, errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDBStore{createErr: tc.createErr}
			h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
			req := httptest.NewRequest(tc.method, "/register", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			h.HandleRegister(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}

	// 成功的 case 要多驗 response body 和存進去的 hash，形狀不同，單獨寫
	t.Run("success returns 201 and stores hash", func(t *testing.T) {
		fake := &fakeDBStore{createID: 7}
		h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(validBody))
		rec := httptest.NewRecorder()

		h.HandleRegister(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var got userResponse
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if want := (userResponse{ID: 7, Username: "jay"}); got != want {
			t.Errorf("response = %+v, want %+v", got, want)
		}

		// spy：handler 傳給 store 的參數
		if fake.gotUsername != "jay" {
			t.Errorf("CreateUser username = %q, want %q", fake.gotUsername, "jay")
		}
		// 存的不能是明文，而且要是「這個密碼」的 hash
		if fake.gotHash == "kq7vz2mx" {
			t.Fatalf("CreateUser got plaintext password, want bcrypt hash")
		}
		ok, err := auth.CheckPassword(fake.gotHash, "kq7vz2mx")
		if err != nil || !ok {
			t.Errorf("CheckPassword(gotHash, password) = %v, %v, want true, nil", ok, err)
		}
	})
}

func TestHandleLogin(t *testing.T) {
	const password = "kq7vz2mx"
	// fake 裡要放真的 bcrypt hash，handler 才能用 CheckPassword 比對
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	// DB 存的是 "Jay"，login 用 "jay"：成功時要回 DB 存的大小寫
	stored := model.User{ID: 7, Username: "Jay", PasswordHash: hash}
	body := func(username, password string) string {
		return `{"username":"` + username + `","password":"` + password + `"}`
	}

	cases := []struct {
		name       string
		method     string
		body       string
		user       model.User
		getErr     error
		wantStatus int
		wantBody   string
	}{
		{"broken json", http.MethodPost, `{"username":"jay",}`, stored, nil, http.StatusBadRequest, ""},
		// 這兩個 case 要回一模一樣的 status 和訊息，不能讓人分辨帳號存不存在
		{"unknown user", http.MethodPost, body("nobody", password), model.User{}, store.ErrUserNotFound, http.StatusBadRequest, msgInvalidCredentials},
		{"wrong password", http.MethodPost, body("jay", "wrong-password"), stored, nil, http.StatusBadRequest, msgInvalidCredentials},
		{"db error", http.MethodPost, body("jay", password), model.User{}, errors.New("db down"), http.StatusInternalServerError, ""},
		// DB 裡的 hash 壞掉是系統錯誤，不是密碼錯
		{"corrupt hash", http.MethodPost, body("jay", password), model.User{ID: 7, Username: "Jay", PasswordHash: "not-a-hash"}, nil, http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDBStore{user: tc.user, getErr: tc.getErr}
			h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
			req := httptest.NewRequest(tc.method, "/login", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			h.HandleLogin(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// http.Error 會在訊息後面補一個換行
			if tc.wantBody != "" && strings.TrimSpace(rec.Body.String()) != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
		})
	}

	t.Run("success returns 200 with access token and stored username", func(t *testing.T) {
		fake := &fakeDBStore{user: stored}
		h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body("jay", password)))
		rec := httptest.NewRecorder()

		h.HandleLogin(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var got loginResponse
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if want := (userResponse{ID: 7, Username: "Jay"}); got.User != want {
			t.Errorf("user = %+v, want %+v", got.User, want)
		}
		if got.TokenType != "Bearer" || got.ExpiresIn != 3600 {
			t.Errorf("token_type = %q, expires_in = %d, want Bearer, 3600", got.TokenType, got.ExpiresIn)
		}
		// token 要能驗過，而且代表的是 DB 裡那個 user（不是 request 帶來的 username）
		if userID, err := testTokens.ParseAccessToken(got.AccessToken); err != nil || userID != 7 {
			t.Errorf("ParseAccessToken(access_token) = (%d, %v), want (7, nil)", userID, err)
		}
		// 回給 client 的是明文、存進 DB 的是它的 hash，而且是存給 DB 裡那個 user
		if got.RefreshToken == "" {
			t.Fatalf("refresh_token is empty")
		}
		if fake.gotTokenHash != auth.HashRefreshToken(got.RefreshToken) {
			t.Errorf("stored hash = %q, want HashRefreshToken(refresh_token)", fake.gotTokenHash)
		}
		if fake.gotTokenUserID != 7 {
			t.Errorf("CreateRefreshToken userID = %d, want 7", fake.gotTokenUserID)
		}
	})

	// 存 refresh token 失敗 = server 的錯：跟簽 access token 失敗一樣，不算登入成功
	t.Run("refresh token store failure returns 500 without reset or refund", func(t *testing.T) {
		limiter := &fakeRateLimiter{}
		h := NewAuthHandler(&fakeDBStore{user: stored, createTokenErr: errors.New("db down")}, limiter, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body("jay", password)))
		rec := httptest.NewRecorder()

		h.HandleLogin(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if len(limiter.resetKeys) != 0 || len(limiter.refundKeys) != 0 {
			t.Errorf("Reset keys = %q, Refund keys = %q, want both empty", limiter.resetKeys, limiter.refundKeys)
		}
	})
}

func TestHandleLoginRateLimit(t *testing.T) {
	const password = "kq7vz2mx"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	stored := model.User{ID: 7, Username: "Jay", PasswordHash: hash}
	// httptest.NewRequest 預設的 RemoteAddr 是 192.0.2.1:1234
	const (
		ipKey       = "rl:login:ip:192.0.2.1"
		usernameKey = "rl:login:username:jay"
	)
	// 用大寫登入：帳號的 key 要轉小寫，換大小寫才不能多猜
	body := `{"username":"JAY","password":"` + password + `"}`

	cases := []struct {
		name           string
		method         string
		body           string
		limiter        *fakeRateLimiter
		wantStatus     int
		wantRetryAfter string   // 空字串 = 不檢查
		wantAllowKeys  []string // 依序問過 limiter 的 key
		wantResetKeys  []string
		wantRefundKeys []string
	}{
		// IP 被擋：還沒 decode body 就回 429，不會再問帳號
		{"ip blocked", http.MethodPost, body, &fakeRateLimiter{blockKey: ipKey, retryAfter: 30 * time.Second}, http.StatusTooManyRequests, "30", []string{ipKey}, nil, nil},
		{"username blocked", http.MethodPost, body, &fakeRateLimiter{blockKey: usernameKey, retryAfter: 30 * time.Second}, http.StatusTooManyRequests, "30", []string{ipKey, usernameKey}, nil, nil},
		// 秒數無條件進位：30.4 秒回 31（回 30 的話 client 會早到、又被擋一次）
		{"retry after rounds up", http.MethodPost, body, &fakeRateLimiter{blockKey: ipKey, retryAfter: 30400 * time.Millisecond}, http.StatusTooManyRequests, "31", []string{ipKey}, nil, nil},
		// 剩 0 秒也至少回 1，不能叫 client 現在就再試
		{"retry after at least 1", http.MethodPost, body, &fakeRateLimiter{blockKey: ipKey, retryAfter: 0}, http.StatusTooManyRequests, "1", []string{ipKey}, nil, nil},
		// fail open：limiter 壞掉時照常登入
		{"limiter error fails open", http.MethodPost, body, &fakeRateLimiter{err: errors.New("redis down")}, http.StatusOK, "", []string{ipKey, usernameKey}, []string{usernameKey}, []string{ipKey}},
		// 成功：帳號 reset、IP 只退還一次（reset IP 的話，攻擊者用自己的帳號登入一次就能把 IP 計數歸零）
		{"success resets username and refunds ip", http.MethodPost, body, &fakeRateLimiter{}, http.StatusOK, "", []string{ipKey, usernameKey}, []string{usernameKey}, []string{ipKey}},
		// 失敗的不退：IP 規則等於只數失敗
		{"wrong password no reset or refund", http.MethodPost, `{"username":"JAY","password":"wrong-password"}`, &fakeRateLimiter{}, http.StatusBadRequest, "", []string{ipKey, usernameKey}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(&fakeDBStore{user: stored}, tc.limiter, "", testTokens)
			req := httptest.NewRequest(tc.method, "/login", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			h.HandleLogin(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// 用 Result().Header：它是 WriteHeader 當下送出去的 header；rec.Header() 之後再 Set 也看得到，會漏掉「太晚設」的 bug
			if got := rec.Result().Header.Get("Retry-After"); tc.wantRetryAfter != "" && got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
			if !slices.Equal(tc.limiter.allowKeys, tc.wantAllowKeys) {
				t.Errorf("Allow keys = %q, want %q", tc.limiter.allowKeys, tc.wantAllowKeys)
			}
			if !slices.Equal(tc.limiter.resetKeys, tc.wantResetKeys) {
				t.Errorf("Reset keys = %q, want %q", tc.limiter.resetKeys, tc.wantResetKeys)
			}
			if !slices.Equal(tc.limiter.refundKeys, tc.wantRefundKeys) {
				t.Errorf("Refund keys = %q, want %q", tc.limiter.refundKeys, tc.wantRefundKeys)
			}
		})
	}

	// username 格式不合法：IP 限流照算（算一次失敗，不 Refund），但回 400 後不碰 username 限流（不能變成 Redis key）、不查 DB
	for name, username := range map[string]string{
		"overlong username":     strings.Repeat("a", 1000),
		"bad chars in username": "bad name!",
	} {
		t.Run(name, func(t *testing.T) {
			limiter := &fakeRateLimiter{}
			db := &fakeDBStore{user: stored}
			h := NewAuthHandler(db, limiter, "", testTokens)
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
			rec := httptest.NewRecorder()

			h.HandleLogin(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != msgInvalidCredentials {
				t.Errorf("body = %q, want %q", got, msgInvalidCredentials)
			}
			if !slices.Equal(limiter.allowKeys, []string{ipKey}) {
				t.Errorf("Allow keys = %q, want only %q", limiter.allowKeys, []string{ipKey})
			}
			if len(limiter.resetKeys) != 0 || len(limiter.refundKeys) != 0 {
				t.Errorf("Reset keys = %q, Refund keys = %q, want both empty", limiter.resetKeys, limiter.refundKeys)
			}
			if db.getUserCalled {
				t.Error("GetUser was called, want not called")
			}
		})
	}

	// IP 已被擋時，即使 username 超長也是 429（IP 限流在 decode / 格式檢查之前）
	t.Run("ip blocked beats invalid username", func(t *testing.T) {
		limiter := &fakeRateLimiter{blockKey: ipKey, retryAfter: 30 * time.Second}
		db := &fakeDBStore{user: stored}
		h := NewAuthHandler(db, limiter, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"`+strings.Repeat("a", 1000)+`","password":"`+password+`"}`))
		rec := httptest.NewRecorder()

		h.HandleLogin(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if !slices.Equal(limiter.allowKeys, []string{ipKey}) {
			t.Errorf("Allow keys = %q, want only %q", limiter.allowKeys, []string{ipKey})
		}
		if db.getUserCalled {
			t.Error("GetUser was called, want not called")
		}
	})

	// 簽 token 失敗 = server 的錯：回 500，而且不算登入成功（不 Reset 帳號、不退還 IP）
	t.Run("token issue failure returns 500 without reset or refund", func(t *testing.T) {
		limiter := &fakeRateLimiter{}
		h := NewAuthHandler(&fakeDBStore{user: stored}, limiter, "", failingTokenIssuer{})
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		rec := httptest.NewRecorder()

		h.HandleLogin(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if len(limiter.resetKeys) != 0 || len(limiter.refundKeys) != 0 {
			t.Errorf("Reset keys = %q, Refund keys = %q, want both empty", limiter.resetKeys, limiter.refundKeys)
		}
	})

	// 有設定要信任的 header 時，IP 的 key 要用 header 的值（確認 trustedHeaderKey 有接到 httpx.ClientIP）
	t.Run("ip from trusted header", func(t *testing.T) {
		limiter := &fakeRateLimiter{}
		h := NewAuthHandler(&fakeDBStore{user: stored}, limiter, "X-Real-IP", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.Header.Set("X-Real-IP", "1.2.3.4")
		rec := httptest.NewRecorder()

		h.HandleLogin(rec, req)

		if len(limiter.allowKeys) == 0 || limiter.allowKeys[0] != "rl:login:ip:1.2.3.4" {
			t.Errorf("Allow keys = %q, want first %q", limiter.allowKeys, "rl:login:ip:1.2.3.4")
		}
	})
}

func TestHandleRegisterRateLimit(t *testing.T) {
	const (
		validBody = `{"username":"jay","password":"kq7vz2mx"}`
		ipKey     = "rl:register:ip:192.0.2.1"
	)

	t.Run("ip blocked", func(t *testing.T) {
		fake := &fakeDBStore{createID: 7}
		limiter := &fakeRateLimiter{blockKey: ipKey, retryAfter: time.Hour}
		h := NewAuthHandler(fake, limiter, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(validBody))
		rec := httptest.NewRecorder()

		h.HandleRegister(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d (body: %q)", rec.Code, http.StatusTooManyRequests, rec.Body.String())
		}
		if got := rec.Result().Header.Get("Retry-After"); got != "3600" {
			t.Errorf("Retry-After = %q, want %q", got, "3600")
		}
		// 被擋就不能走到建帳號
		if fake.gotUsername != "" {
			t.Errorf("CreateUser called with %q, want not called", fake.gotUsername)
		}
	})

	t.Run("limiter error fails open", func(t *testing.T) {
		limiter := &fakeRateLimiter{err: errors.New("redis down")}
		h := NewAuthHandler(&fakeDBStore{createID: 7}, limiter, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(validBody))
		rec := httptest.NewRecorder()

		h.HandleRegister(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if !slices.Equal(limiter.allowKeys, []string{ipKey}) {
			t.Errorf("Allow keys = %q, want %q", limiter.allowKeys, []string{ipKey})
		}
	})
}

func TestHandleRefresh(t *testing.T) {
	const validBody = `{"refresh_token":"old-plain"}`

	cases := []struct {
		name       string
		method     string
		body       string
		rotateErr  error
		wantStatus int
	}{
		{"broken json", http.MethodPost, `{"refresh_token":`, nil, http.StatusBadRequest},
		{"missing token", http.MethodPost, `{}`, nil, http.StatusBadRequest},
		{"invalid token", http.MethodPost, validBody, store.ErrRefreshTokenInvalid, http.StatusUnauthorized},
		// duplicate use 對 client 也是 401：不透露「這個 token 曾經有效」
		{"duplicate use token", http.MethodPost, validBody, &store.RefreshTokenDuplicateUseError{UserID: 7}, http.StatusUnauthorized},
		{"db error", http.MethodPost, validBody, errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(&fakeDBStore{rotateUserID: 7, rotateErr: tc.rotateErr}, &fakeRateLimiter{}, "", testTokens)
			req := httptest.NewRequest(tc.method, "/refresh", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			h.HandleRefresh(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}

	t.Run("success returns new tokens and rotates by hash", func(t *testing.T) {
		fake := &fakeDBStore{rotateUserID: 7}
		h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(validBody))
		rec := httptest.NewRecorder()

		h.HandleRefresh(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		// store 收到的是 hash，不是明文
		if fake.gotOldHash != auth.HashRefreshToken("old-plain") {
			t.Errorf("Rotate oldHash = %q, want HashRefreshToken(old-plain)", fake.gotOldHash)
		}
		var got tokenResponse
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got.RefreshToken == "" || got.RefreshToken == "old-plain" {
			t.Errorf("refresh_token = %q, want a new token", got.RefreshToken)
		}
		if userID, err := testTokens.ParseAccessToken(got.AccessToken); err != nil || userID != 7 {
			t.Errorf("ParseAccessToken(access_token) = (%d, %v), want (7, nil)", userID, err)
		}
	})

	t.Run("ip blocked", func(t *testing.T) {
		fake := &fakeDBStore{rotateUserID: 7}
		limiter := &fakeRateLimiter{blockKey: "rl:refresh:ip:192.0.2.1", retryAfter: time.Minute}
		h := NewAuthHandler(fake, limiter, "", testTokens)
		req := httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(validBody))
		rec := httptest.NewRecorder()

		h.HandleRefresh(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if fake.gotOldHash != "" {
			t.Errorf("Rotate called after rate limited, want not called")
		}
	})
}

func TestHandleLogout(t *testing.T) {
	const validBody = `{"refresh_token":"old-plain"}`

	cases := []struct {
		name       string
		method     string
		body       string
		revokeErr  error
		wantStatus int
	}{
		{"broken json", http.MethodPost, `{"refresh_token":`, nil, http.StatusBadRequest},
		{"missing token", http.MethodPost, `{}`, nil, http.StatusBadRequest},
		// store 對查不到 / 已作廢都回 nil，handler 一律 204
		{"success", http.MethodPost, validBody, nil, http.StatusNoContent},
		// DB 壞了不能回 204：client 會以為登出了
		{"db error", http.MethodPost, validBody, errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDBStore{revokeErr: tc.revokeErr}
			h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
			req := httptest.NewRequest(tc.method, "/logout", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			h.HandleLogout(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusNoContent && fake.gotRevokeHash != auth.HashRefreshToken("old-plain") {
				t.Errorf("Revoke hash = %q, want HashRefreshToken(old-plain)", fake.gotRevokeHash)
			}
		})
	}
}

// 用 RequireAuth 包起來送 Bearer token，user id 才會進到 ctx（context key 沒有也不該 export setter）
func serveMe(t *testing.T, h *AuthHandler, userID int64) *httptest.ResponseRecorder {
	t.Helper()
	token, _, err := testTokens.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	wrapped := middleware.RequireAuth(testTokens)(http.HandlerFunc(h.HandleMe))
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

func TestHandleMe(t *testing.T) {
	const userID = 7

	t.Run("returns current user", func(t *testing.T) {
		fake := &fakeDBStore{userByID: model.User{ID: userID, Username: "jay", PasswordHash: "secret-hash"}}
		h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)

		rec := serveMe(t, h, userID)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"id":7,"username":"jay"}` {
			t.Errorf("body = %q, want %q", got, `{"id":7,"username":"jay"}`)
		}
		if fake.gotByIDCall != userID {
			t.Errorf("GetUserByID id = %d, want %d", fake.gotByIDCall, userID)
		}
	})

	t.Run("deleted user returns same 401 as RequireAuth", func(t *testing.T) {
		h := NewAuthHandler(&fakeDBStore{getByIDErr: store.ErrUserNotFound}, &fakeRateLimiter{}, "", testTokens)

		rec := serveMe(t, h, userID)

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
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("db error returns 500", func(t *testing.T) {
		h := NewAuthHandler(&fakeDBStore{getByIDErr: errors.New("db down")}, &fakeRateLimiter{}, "", testTokens)

		rec := serveMe(t, h, userID)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	// handler 沒經過 RequireAuth（route 註冊錯）：fail closed 回 500，不查 DB
	t.Run("missing user id in context returns 500", func(t *testing.T) {
		fake := &fakeDBStore{}
		h := NewAuthHandler(fake, &fakeRateLimiter{}, "", testTokens)
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		rec := httptest.NewRecorder()

		h.HandleMe(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if fake.gotByIDCall != 0 {
			t.Error("queried DB without a user id")
		}
	})
}
