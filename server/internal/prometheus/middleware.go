package prometheus

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	promlib "github.com/prometheus/client_golang/prometheus"
)

func HttpMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := middleware.NewStatusCodeRecorder(w)
		start := time.Now()

		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		// r.Pattern 是 mux 在 next.ServeHTTP 裡填的，所以要在這之後才讀（前提見 main.go serviceHandler）
		method, path := requestLabels(r)

		HttpRequestCount.With(promlib.Labels{
			"method":      method,
			"path":        path,
			"status_code": strconv.Itoa(recorder.StatusCode()),
		}).Inc()
		HttpRequestDuration.With(promlib.Labels{
			"method": method,
			"path":   path,
		}).Observe(duration.Seconds())
	})
}

// metrics 的 method / path label；兩個值都已經收斂成有限集合，見 getMethod、getRoutePath
func requestLabels(r *http.Request) (method, path string) {
	return getMethod(r.Method), getRoutePath(r.Pattern)
}

// 把 r.Method 轉成 metrics 的 method label：跟 getRoutePath 同一個理由，label 的值不能由 client 決定
// net/http 接受任何合法 token 當 method（例如 "FOOBAR123"），直接用 r.Method 的話換個字串就多一條 series
//
// HTTP 標準的九個 method → 原樣（HEAD 是 healthcheck、OPTIONS 是 CORS preflight，都是正常流量，不能跟亂打的混在一起）
// 其他（含小寫的 "get"，method 有分大小寫）→ "other"
func getMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "other"
	}
}

// 把 r.Pattern（mux 比對到的 route）轉成 metrics 的 path label
// label 只能用我們自己註冊的 pattern（數量有限），不能用 r.URL.Path（client 亂打就會一直長新的 series）
//
// "GET /rooms/{public_id}" → "/rooms/{public_id}"（去掉 method，method 已經有自己的 label）
// "/messages"              → "/messages"（removed() 註冊時不帶 method）
// ""                       → "unmatched"（沒比對到 route：404、405、CORS preflight）
func getRoutePath(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	// 用第一個空白切開："GET /rooms/{public_id}" → before = "GET"、after = "/rooms/{public_id}"
	_, path, found := strings.Cut(pattern, " ")
	if found {
		// 有空白 = 帶 method：要的是後面的 path
		return path
	}
	// 沒有空白 = 沒帶 method：整個 pattern 就是 path
	return pattern
}
