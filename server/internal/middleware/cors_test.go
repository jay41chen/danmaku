package middleware

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCORS(t *testing.T) {
	cases := []struct {
		name         string
		allowOrigins []string
		origin       string // request 的 Origin header，"" 表示不帶
		wantAllow    string // 預期的 Access-Control-Allow-Origin，"" 表示不該有
		wantVary     bool
		wantExpose   string // 預期的 Access-Control-Expose-Headers，"" 表示不該有
	}{
		{"no origin", []string{"https://a.com"}, "", "", false, ""},
		{"empty list", []string{}, "https://a.com", "", true, ""},
		{"allow all", []string{"*"}, "https://a.com", "*", false, "Retry-After"},
		{"in list", []string{"https://a.com", "https://b.com"}, "https://b.com", "https://b.com", true, "Retry-After"},
		{"not in list", []string{"https://a.com"}, "https://evil.com", "", true, ""},
		{"trailing slash mismatch", []string{"https://a.com/"}, "https://a.com", "", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
			})
			handler := CORS(tc.allowOrigins...)(next)

			req := httptest.NewRequest(http.MethodGet, "/messages", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// CORS 不擋 request：不管允不允許，next 都要被呼叫
			if !nextCalled {
				t.Error("next handler was not called")
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got := rec.Header().Get("Vary") == "Origin"; got != tc.wantVary {
				t.Errorf("Vary: Origin = %v, want %v", got, tc.wantVary)
			}
			if got := rec.Header().Get("Access-Control-Expose-Headers"); got != tc.wantExpose {
				t.Errorf("Expose-Headers = %q, want %q", got, tc.wantExpose)
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	allowed := []string{"https://a.com"}

	// 基準是 "valid preflight"，後面每個 case 只改一個條件
	cases := []struct {
		name          string
		allowOrigins  []string
		method        string
		origin        string // "" 表示不帶
		requestMethod string // Access-Control-Request-Method，"" 表示不帶
		wantStatus    int
		wantNext      bool   // false = 被當成 preflight 攔下
		wantAllow     string // 預期的 Access-Control-Allow-Origin
		wantExpose    string // 預期的 Access-Control-Expose-Headers；preflight 一律 ""，只有放行的實際 request 且 origin 被允許才有
	}{
		{"valid preflight", allowed, http.MethodOptions, "https://a.com", "POST", http.StatusNoContent, false, "https://a.com", ""},
		{"not OPTIONS", allowed, http.MethodGet, "https://a.com", "POST", http.StatusOK, true, "https://a.com", "Retry-After"},
		{"no origin", allowed, http.MethodOptions, "", "POST", http.StatusOK, true, "", ""},
		{"no request method", allowed, http.MethodOptions, "https://a.com", "", http.StatusOK, true, "https://a.com", "Retry-After"},
		// 仍然攔下回 204，但沒有 Allow-Origin → 瀏覽器不會送出真正的 request
		{"origin not in list", allowed, http.MethodOptions, "https://evil.com", "POST", http.StatusNoContent, false, "", ""},
		{"allow all", []string{"*"}, http.MethodOptions, "https://a.com", "POST", http.StatusNoContent, false, "*", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
			})
			handler := CORS(tc.allowOrigins...)(next)

			req := httptest.NewRequest(tc.method, "/messages", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.requestMethod != "" {
				req.Header.Set("Access-Control-Request-Method", tc.requestMethod)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if nextCalled != tc.wantNext {
				t.Errorf("next called = %v, want %v", nextCalled, tc.wantNext)
			}

			// 用 Result().Header：WriteHeader 當下的快照 = client 真正收到的。
			// rec.Header() 是還能改的 map，WriteHeader 之後才 Set 的也看得到，會誤判通過
			got := rec.Result().Header
			if v := got.Get("Access-Control-Allow-Origin"); v != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", v, tc.wantAllow)
			}

			// Expose-Headers 只給實際 request；preflight 被攔下時不能有
			if v := got.Get("Access-Control-Expose-Headers"); v != tc.wantExpose {
				t.Errorf("Expose-Headers = %q, want %q", v, tc.wantExpose)
			}

			// header 名稱寫字面值而不是用 cors.go 的 const：
			// const 打錯（如 "Allow-methods"）時，test 才抓得到
			wantPreflight := map[string]string{
				"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
				"Access-Control-Allow-Headers": "Content-Type, Authorization",
				"Access-Control-Max-Age":       "600",
			}
			for name, want := range wantPreflight {
				if tc.wantNext {
					want = "" // 沒被攔下 = 不是 preflight，不該有這些 header
				}
				if v := got.Get(name); v != want {
					t.Errorf("%s = %q, want %q", name, v, want)
				}
			}
		})
	}
}

func TestParseOrigins(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		// 合法
		{"unset defaults to allow all", "", []string{"*"}, false},
		{"allow all", "*", []string{"*"}, false},
		{"single", "https://jay41chen.github.io", []string{"https://jay41chen.github.io"}, false},
		{"with port", "http://localhost:5173", []string{"http://localhost:5173"}, false},
		{"trim spaces and skip empty", " https://a.com , https://b.com, ", []string{"https://a.com", "https://b.com"}, false},

		// 格式錯誤
		{"missing scheme", "jay41chen.github.io", nil, true},
		{"trailing slash", "https://a.com/", nil, true},
		{"with path", "https://jay41chen.github.io/danmaku/", nil, true},
		{"json array", `["https://a.com"]`, nil, true},
		{"one bad among good", "https://a.com,a.com", nil, true},

		// "*" 必須是唯一項目，不管在哪個位置
		{"wildcard first", "*,https://a.com", nil, true},
		{"wildcard middle", "https://a.com,*,https://b.com", nil, true},
		{"wildcard last", "https://a.com,*", nil, true},
		{"duplicate wildcard", "*,*", nil, true},

		// 有設定但沒有任何 origin
		{"only separators", " , ", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOrigins(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseOrigins(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			// slice 不能用 == 比較，要用 slices.Equal
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseOrigins(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// 瀏覽器送的 Origin 一定是 canonical（全小寫、省略預設 port、沒有 query / fragment / userinfo），
// CORS 用字串完全比對，設定不是 canonical 就永遠對不到，所以要在啟動時擋下
func TestParseOriginsNonCanonical(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"uppercase scheme and host", "HTTPS://A.COM", nil, true},
		// scheme 已是小寫時，whitelist 組出的 want 跟原字串相同，只有大寫檢查擋得到
		{"uppercase host only", "https://A.COM", nil, true},
		{"https default port", "https://a.com:443", nil, true},
		{"http default port", "http://a.com:80", nil, true},
		{"with query", "https://a.com?x=1", nil, true},
		{"with fragment", "https://a.com#top", nil, true},
		{"with userinfo", "https://user@a.com", nil, true},

		// 欄位是空的但字串多了符號：逐欄檢查會漏，要靠跟 scheme://host 完全比對
		{"empty query", "https://a.com?", nil, true},
		{"empty fragment", "https://a.com#", nil, true},
		{"empty port", "https://a.com:", nil, true},

		// 非預設 port 瀏覽器會保留，必須照樣通過（確認檢查沒有擋過頭）
		{"non-default port", "https://a.com:8443", []string{"https://a.com:8443"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOrigins(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseOrigins(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseOrigins(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
