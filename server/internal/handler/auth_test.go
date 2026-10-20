package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/auth"
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
		{"GET not allowed", http.MethodGet, "", nil, http.StatusMethodNotAllowed},
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
			h := NewAuthHandler(fake)
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
		h := NewAuthHandler(fake)
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
