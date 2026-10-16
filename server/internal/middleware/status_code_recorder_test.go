package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 重複 WriteHeader 只有第一次生效，recorder 要記第一次（跟 client 實際收到的一致）
func TestMultipleSettingStatusCode(t *testing.T) {
	r := httptest.NewRecorder()
	rec := NewStatusCodeRecorder(r)

	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.StatusCode() != http.StatusOK {
		t.Errorf("expected recorded status %d, got %d", http.StatusOK, rec.StatusCode())
	}
	// 確認 recorder 記的跟 client 實際收到的一致
	if r.Code != rec.StatusCode() {
		t.Errorf("recorded %d but client got %d", rec.StatusCode(), r.Code)
	}
}

// 先 Write 會隱含送出 200，之後的 WriteHeader(500) 不生效
func TestWriteBeforeWriteHeader(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := NewStatusCodeRecorder(underlying)

	rec.Write([]byte("hello"))
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.StatusCode() != http.StatusOK {
		t.Errorf("expected recorded status %d, got %d", http.StatusOK, rec.StatusCode())
	}
	if underlying.Code != rec.StatusCode() {
		t.Errorf("recorded %d but client got %d", rec.StatusCode(), underlying.Code)
	}
}
