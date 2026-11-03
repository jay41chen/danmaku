package prometheus

import (
	"net/http"
	"testing"
)

// mux 的 r.Pattern 轉成 metric 的 path label：去掉 method、沒 match 一律歸成 unmatched
func TestGetRoutePath(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"GET /rooms/{public_id}", "/rooms/{public_id}"},
		{"POST /rooms/{public_id}/messages", "/rooms/{public_id}/messages"},
		{"/messages", "/messages"},
		{"", "unmatched"},
	}
	for _, tt := range tests {
		if got := getRoutePath(tt.pattern); got != tt.want {
			t.Errorf("getRoutePath(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}
}

// method label 只放標準的九個，其他（含小寫，method 有分大小寫）一律歸成 other
func TestGetMethod(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{http.MethodGet, http.MethodGet},
		{http.MethodHead, http.MethodHead},
		{http.MethodPost, http.MethodPost},
		{http.MethodPut, http.MethodPut},
		{http.MethodPatch, http.MethodPatch},
		{http.MethodDelete, http.MethodDelete},
		{http.MethodConnect, http.MethodConnect},
		{http.MethodOptions, http.MethodOptions},
		{http.MethodTrace, http.MethodTrace},
		{"FOOBAR123", "other"},
		{"get", "other"},
		{"", "other"},
	}
	for _, tt := range tests {
		if got := getMethod(tt.method); got != tt.want {
			t.Errorf("getMethod(%q) = %q, want %q", tt.method, got, tt.want)
		}
	}
}
