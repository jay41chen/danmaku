package middleware

import (
	"bufio"
	"net"
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

// httptest 會忽略第二次 WriteHeader，看不出 recorder 有沒有轉發，所以自己計次
type countingWriter struct {
	*httptest.ResponseRecorder
	writeHeaderCalls int
}

func (w *countingWriter) WriteHeader(code int) {
	w.writeHeaderCalls++
	w.ResponseRecorder.WriteHeader(code)
}

// recorder 只記第一次，但每次都要轉發：拿掉 recorder 時 net/http 的行為要一樣
// （net/http 收到第二次會印 "superfluous response.WriteHeader call"，吞掉就看不到這個 bug 警告）
func TestWriteHeaderAlwaysForwarded(t *testing.T) {
	cw := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	rec := NewStatusCodeRecorder(cw)

	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusInternalServerError)
	want := 2
	if cw.writeHeaderCalls != want {
		t.Errorf("expected callCount %d, got %d", want, cw.writeHeaderCalls)
	}
}

// httptest 沒有實作 http.Hijacker；這個 fake 扮演 net/http，假裝 Hijack 成功（err == nil）
type hijackableWriter struct {
	*httptest.ResponseRecorder
}

func (w *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

// Hijack 後 101 已經由 gorilla 送出，之後的 WriteHeader（例如 RecoverPanic 的 500）不能覆蓋
func TestWriteHeaderAfterHijack(t *testing.T) {
	fake := &hijackableWriter{ResponseRecorder: httptest.NewRecorder()}
	rec := NewStatusCodeRecorder(fake)

	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("hijack failed: %v", err)
	}
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.StatusCode() != http.StatusSwitchingProtocols {
		t.Errorf("expected recorded status %d, got %d", http.StatusSwitchingProtocols, rec.StatusCode())
	}
}

// Hijack 後連線已交給 handler，Committed 要回報 true（RecoverPanic 靠它決定要不要再寫 500）
func TestCommittedAfterHijack(t *testing.T) {
	fake := &hijackableWriter{ResponseRecorder: httptest.NewRecorder()}
	rec := NewStatusCodeRecorder(fake)

	if rec.Committed() {
		t.Error("expected Committed() == false before hijack")
	}
	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("hijack failed: %v", err)
	}
	if !rec.Committed() {
		t.Error("expected Committed() == true after hijack")
	}
}

// WriteHeader 或 Write（隱含 200）之後 Committed 為 true，之前為 false
func TestCommitted(t *testing.T) {
	t.Run("fresh recorder", func(t *testing.T) {
		rec := NewStatusCodeRecorder(httptest.NewRecorder())
		if rec.Committed() {
			t.Error("expected Committed() == false")
		}
	})
	t.Run("after WriteHeader", func(t *testing.T) {
		rec := NewStatusCodeRecorder(httptest.NewRecorder())
		rec.WriteHeader(http.StatusNoContent)
		if !rec.Committed() {
			t.Error("expected Committed() == true")
		}
	})
	t.Run("after Write without WriteHeader", func(t *testing.T) {
		rec := NewStatusCodeRecorder(httptest.NewRecorder())
		if _, err := rec.Write([]byte("x")); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		if !rec.Committed() {
			t.Error("expected Committed() == true")
		}
	})
}
