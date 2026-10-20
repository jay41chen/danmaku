package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

func TestHandleMessages(t *testing.T) {
	t.Run("db error returns 500", func(t *testing.T) {
		h := NewMessageHandler(&fakeDBStore{err: errors.New("db down")})
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleMessages(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
	t.Run("returns oldest first", func(t *testing.T) {
		input := []model.Message{{Content: "c"},
			{Content: "b"},
			{Content: "a"}}
		// copy一份input並翻轉，這是我們期待的結果
		expect := slices.Clone(input)
		slices.Reverse(expect)
		h := NewMessageHandler(&fakeDBStore{msgs: input})
		req := httptest.NewRequest(http.MethodGet, "/messages", nil)
		rec := httptest.NewRecorder()

		h.HandleMessages(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var got []model.Message
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}

		if !slices.Equal(expect, got) {
			t.Errorf("expect = %q, got %q", expect, got)
		}
	})
}
