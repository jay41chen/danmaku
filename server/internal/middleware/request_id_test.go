package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsValidRequestID(t *testing.T) {
	cases := map[string]bool{
		"abc-123":               true,
		"":                      false,
		"bad id!":               false,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
	}
	for input, want := range cases {
		if got := isValidRequestID(input); got != want {
			t.Errorf("isValidRequestID(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestGenerateValidID(t *testing.T) {
	for range 100 {
		id := generateRequestID()
		if !isValidRequestID(id) {
			t.Errorf("not generating valid id(%q)", id)
		}
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	cases := []struct {
		name   string
		header string // "" 代表 client 沒帶
		keep   bool   // true：應該沿用 client 帶的 ID
	}{
		{"valid header kept", "abc-123", true},
		{"invalid header replaced", "bad id!", false},
		{"missing header generated", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ctxID string
			mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = RequestIDFromContext(r.Context())
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				req.Header.Set(RequestIDHeader, c.header)
			}
			rec := httptest.NewRecorder()
			RequestID(mock).ServeHTTP(rec, req)
			resID := rec.Header().Get(RequestIDHeader)

			// 萬一response id跟ctx id不同，就一定出事
			if resID != ctxID {
				t.Errorf("response id %q != ctx id %q", resID, ctxID)
			}
			// 如果本來就要沿用
			if c.keep {
				if resID != c.header {
					t.Errorf("want kept %q, got %q", c.header, resID)
				}
				// 如果本來就要換掉
			} else {
				// 換掉：輸出不能等於輸入，而且要合法
				if resID == c.header || !isValidRequestID(resID) {
					t.Errorf("want new valid id replacing %q, got %q", c.header, resID)
				}
			}
		})
	}
}
