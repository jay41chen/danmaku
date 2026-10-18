package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	headerOrigin        = "Origin"
	headerAllowOrigin   = "Access-Control-Allow-Origin"
	headerRequestMethod = "Access-Control-Request-Method"

	headerAllowMethods = "Access-Control-Allow-Methods"
	corsAllowMethods   = "GET, POST, OPTIONS"
	headerAllowHeaders = "Access-Control-Allow-Headers"
	corsAllowHeaders   = "Content-Type, Authorization"
	headerMaxAge       = "Access-Control-Max-Age"
	corsMaxAge         = "600"
)

// CORS 告訴「瀏覽器」哪些網站的 JS 可以讀這個 server 的 response。
//
// 背景：瀏覽器有 Same-Origin Policy，evil.com 的 JS 不能讀 api.bank.com 的 response
// （否則它可以帶著你的 cookie 偷資料）。CORS 是 server 開的「例外清單」。
//
// 原則：CORS 不擋 request，只加 header。真正擋的是瀏覽器。
//   - curl / OkHttp / server-to-server 完全不受影響（沒有 Same-Origin Policy）
//   - 所以 CORS 不是 server 的安全機制；要保護 API 要靠 JWT / RBAC
//   - origin 不在清單 → 不加 header，request 照樣進 handler，只是瀏覽器不讓 JS 讀結果
//
// 瀏覽器的兩種 request：
//
//	Simple       一般 GET              直接送，看 response 有沒有 Allow-Origin 決定給不給 JS 讀
//	Preflight    POST + Authorization  先送 OPTIONS + Origin + Access-Control-Request-Method 詢問，
//	             （非 simple 的都是）  server 回 204 + 允許清單，瀏覽器同意後才送真正的 request
//	無 Origin    curl / 同源 request   不是跨域，不用處理
//
// Allowlist 規則（HTTP 限制：Allow-Origin 只能放「一個」值，不能寫 "a.com, b.com"）：
//
//	清單有 "*"            回 "*"，所有網站都允許
//	Origin 在清單裡       原樣回寫這個 origin
//	Origin 不在清單裡     不加 Allow-Origin
//
// 清單含 "*" 時其餘項目無效（等於全部允許），混用通常是設定錯誤，應在解析 env 時擋下。
//
// 不是 "*" 時要加 Vary: Origin：同一個 URL 的 response 會依 Origin 不同而不同，
// 告訴 CDN / 瀏覽器快取不能把 a.com 的 response 拿給 b.com 用。
func CORS(allowOrigins ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return cors(next, allowOrigins)
	}
}

func cors(next http.Handler, allowOrigins []string) http.Handler {
	allowAll := slices.Contains(allowOrigins, "*")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get(headerOrigin)
		switch {
		case origin == "":
			// ignore
		case allowAll:
			w.Header().Set(headerAllowOrigin, "*")
		default:
			// 不管有沒有比對到都要加：response 會依 Origin 不同而不同
			w.Header().Add("Vary", headerOrigin)
			if slices.Contains(allowOrigins, origin) {
				w.Header().Set(headerAllowOrigin, origin)
			}
		}
		if isPreflight(r) {
			// 加上對應的header再送出204
			// 特別要注意，WriteHeader後就會送出
			// 所以一定要在最後
			w.Header().Set(headerAllowMethods, corsAllowMethods)
			w.Header().Set(headerAllowHeaders, corsAllowHeaders)
			w.Header().Set(headerMaxAge, corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get(headerOrigin) != "" &&
		r.Header.Get(headerRequestMethod) != ""
}

// ParseOrigins 把 CORS_ORIGINS env（逗號分隔）解析成 CORS 用的 allowlist。
//
// 設定錯誤時回傳 error，讓 server 啟動失敗（fail fast），而不是上線後瀏覽器才默默擋掉：
//
//	""                               → ["*"]（沒設定 = 全部允許）
//	"https://a.com, https://b.com,"  → ["https://a.com", "https://b.com"]（去空白、忽略空項目）
//	"jay41chen.github.io"            → error：缺 scheme
//	"https://a.com/danmaku/"         → error：不能有 path（Origin 只有 scheme + host + port）
//	"https://A.com"                  → error：要全小寫（瀏覽器送的 Origin 是 canonical，比對是字串完全相等）
//	"https://a.com:443"              → error：不能寫預設 port
//	"https://a.com,*"                → error："*" 必須是唯一項目
//	" , "                            → error：有設定但沒有任何 origin
func ParseOrigins(raw string) ([]string, error) {
	if raw == "" {
		// 空字串，自動視為allow all
		return []string{"*"}, nil
	}

	origins := []string{}
	for item := range strings.SplitSeq(raw, ",") {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		if s != "*" {
			// "*" 不是 URL，不用 validate；混用的檢查在 loop 之後
			if err := validateOrigin(s); err != nil {
				return nil, err
			}
		}
		origins = append(origins, s)
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("no valid origin in %q", raw)
	}
	if slices.Contains(origins, "*") && len(origins) > 1 {
		return nil, fmt.Errorf("%q must be the only entry, got %q", "*", origins)
	}
	return origins, nil
}

func validateOrigin(str string) error {
	if strings.ToLower(str) != str {
		return fmt.Errorf("origin %q: shouldn't have uppercase url", str)
	}
	u, err := url.Parse(str)
	if err != nil {
		// 直接無法parse出url
		return fmt.Errorf("origin %q: %w", str, err)
	}
	if u.Scheme == "" || u.Host == "" {
		// scheme跟host parse不出來
		return fmt.Errorf("origin %q: missing scheme or host", str)
	}
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		// 預設的port就不用特別寫了
		return fmt.Errorf("origin %q: browsers omit default port %s", str, u.Port())
	}
	if strings.HasSuffix(u.Host, ":") {
		// ex: abc.com:
		return fmt.Errorf("origin %q: empty port", str)
	}
	if u.Path != "" {
		return fmt.Errorf("origin %q: must not contain a path (got %q)", str, u.Path)
	}
	if u.Fragment != "" || u.RawQuery != "" || u.User != nil {
		// 不應該有fragment/query/user
		return fmt.Errorf("origin %q: cannot have query, fragment, or user", str)
	}
	if want := u.Scheme + "://" + u.Host; str != want {
		// 只用 scheme + host 組回去，跟原字串一樣才放行
		// （擋 "a.com?" 這種欄位是空的但多了符號的寫法）
		return fmt.Errorf("origin %q: not canonical, use %q", str, want)
	}
	return nil
}
