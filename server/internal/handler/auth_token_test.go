package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/model"
)

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
		{"invalid token", http.MethodPost, validBody, model.ErrRefreshTokenInvalid, http.StatusUnauthorized},
		// duplicate use 對 client 也是 401：不透露「這個 token 曾經有效」
		{"duplicate use token", http.MethodPost, validBody, &model.RefreshTokenDuplicateUseError{UserID: 7}, http.StatusUnauthorized},
		// 被撤銷全部之前發的 token：對 client 跟 invalid 一樣是 401（handler 另外記 Warn）
		{"revoked token", http.MethodPost, validBody, &model.RefreshTokenRevokedError{UserID: 7}, http.StatusUnauthorized},
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

// 守住 /refresh 的 log 分支：revoked / duplicate use 要記 Warn（含 user_id、client_ip），
// invalid 不記；任何情況都不能把明文 token 或它的 hash 寫進 log
// 另外開 test 而不是塞進上面的表格：表格 test 只看 status，這裡要把 logger 換成寫進 buffer 的
func TestHandleRefreshLogging(t *testing.T) {
	const plain = "old-plain"
	body := `{"refresh_token":"` + plain + `"}`

	cases := []struct {
		name      string
		rotateErr error
		wantMsg   string // 空字串 = 不應該有 WARN
	}{
		{"revoked token", &model.RefreshTokenRevokedError{UserID: 7}, "refresh token issued before credentials revoked was used"},
		{"duplicate use token", &model.RefreshTokenDuplicateUseError{UserID: 7}, "refresh token duplicate use detected, revoked all tokens of user"},
		{"invalid token", model.ErrRefreshTokenInvalid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := NewAuthHandler(&fakeDBStore{rotateUserID: 7, rotateErr: tc.rotateErr}, &fakeRateLimiter{}, "", testTokens)
			h.logger = slog.New(slog.NewJSONHandler(&buf, nil))
			req := httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.HandleRefresh(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}

			var warns []map[string]any
			dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
			for dec.More() {
				var entry map[string]any
				if err := dec.Decode(&entry); err != nil {
					t.Fatalf("decode log line: %v (log: %s)", err, buf.String())
				}
				if entry["level"] == "WARN" {
					warns = append(warns, entry)
				}
			}

			if tc.wantMsg == "" {
				if len(warns) != 0 {
					t.Errorf("got %d WARN logs, want none: %s", len(warns), buf.String())
				}
			} else {
				if len(warns) != 1 {
					t.Fatalf("got %d WARN logs, want 1: %s", len(warns), buf.String())
				}
				w := warns[0]
				if w["msg"] != tc.wantMsg {
					t.Errorf("msg = %v, want %q", w["msg"], tc.wantMsg)
				}
				// JSON 數字 decode 成 float64
				if w["user_id"] != float64(7) {
					t.Errorf("user_id = %v, want 7", w["user_id"])
				}
				if _, ok := w["client_ip"]; !ok {
					t.Errorf("log missing client_ip: %s", buf.String())
				}
			}

			for _, secret := range []string{plain, auth.HashRefreshToken(plain)} {
				if strings.Contains(buf.String(), secret) {
					t.Errorf("log contains secret %q: %s", secret, buf.String())
				}
			}
		})
	}
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
