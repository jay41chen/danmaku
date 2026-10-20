package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jay41chen/danmaku/server/internal/store"
)

type MessageHandler struct {
	db     store.DBStoreInterface
	logger *slog.Logger
}

// constructor
func NewMessageHandler(db store.DBStoreInterface) *MessageHandler {
	return &MessageHandler{
		db:     db,
		logger: slog.Default().With("component", "messages"),
	}
}

func (this *MessageHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodGet}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	setContentType(w)

	msgs, err := this.db.QueryMessage(r.Context(), 50)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "query messages failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// DB query出來的是新到舊
	slices.Reverse(msgs)
	json.NewEncoder(w).Encode(msgs)
}
