package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverPanic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("expected panic")
	})
	testRecovery := RecoverPanic(mock)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	testRecovery.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want status %d, got %d", http.StatusInternalServerError, rec.Code)
	}
	// panic 的內容（"expected panic"）只進 log，body 是固定的 internal_error
	const wantBody = "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal server error\"}}\n"
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

// handler 先設了成功路徑的 Cache-Control 才 panic：500 要改成 no-store，否則會被 CDN / 共用快取存起來
func TestRecoverPanicOverwritesCacheControl(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, s-maxage=1")
		panic("expected panic")
	})
	rec := httptest.NewRecorder()

	RecoverPanic(mock).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want status %d, got %d", http.StatusInternalServerError, rec.Code)
	}
	if got := rec.Result().Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on the 500", got)
	}
}

func TestRecoverPanicRethrowsAbortHandler(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	testRecovery := RecoverPanic(mock)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("want re-panic with %v, got %v", http.ErrAbortHandler, got)
		}
	}()
	testRecovery.ServeHTTP(rec, req)
}
