package middleware

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestChain(t *testing.T) {
	var order = []string{}
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "mock")
	})
	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler { // 第 2 層：Middleware 本體，收到內圈
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // 第 3 層：真正處理 request
				order = append(order, name+" in")
				next.ServeHTTP(w, r)
				order = append(order, name+" out")
			})
		}
	}
	h := Chain(mock, tag("A"), tag("B"), tag("C"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	want := []string{"A in", "B in", "C in", "mock", "C out", "B out", "A out"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
}
