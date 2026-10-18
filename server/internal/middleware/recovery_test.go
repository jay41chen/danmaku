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
