package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name             string
		trustedHeaderKey string
		headers          map[string]string
		remoteAddr       string
		want             string
	}{
		// 沒設定要信任的 header：client 偽造的值不能被採用
		{"no header configured", "", map[string]string{"X-Real-IP": "6.6.6.6"}, "10.0.0.1:5555", "10.0.0.1"},
		{"header configured", "X-Real-IP", map[string]string{"X-Real-IP": "1.2.3.4"}, "10.0.0.1:5555", "1.2.3.4"},
		// 只能讀設定的那個 header，不能寫死 X-Real-IP：別的 header 再像 IP 都不採用
		{"other platform header", "CF-Connecting-IP", map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Real-IP": "6.6.6.6"}, "10.0.0.1:5555", "1.2.3.4"},
		{"header missing", "X-Real-IP", nil, "10.0.0.1:5555", "10.0.0.1"},
		{"header not an IP", "X-Real-IP", map[string]string{"X-Real-IP": "garbage"}, "10.0.0.1:5555", "10.0.0.1"},
		// 同一個 IP 不同寫法要變成同一個 key（IPv4-mapped IPv6 → IPv4）
		{"header normalized", "X-Real-IP", map[string]string{"X-Real-IP": "::ffff:1.2.3.4"}, "10.0.0.1:5555", "1.2.3.4"},
		// IPv6 的 RemoteAddr 有中括號，要靠 SplitHostPort 去掉
		{"IPv6 remote", "", nil, "[::1]:5555", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			got := ClientIP(req, tc.trustedHeaderKey)

			if got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
