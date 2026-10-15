package logging

import (
	"context"
	"log/slog"

	"github.com/jay41chen/danmaku/server/internal/middleware"
)

// ContextHandler 從 ctx 取出 request-scoped 資料（如 request_id）加進每筆 log。
type ContextHandler struct {
	slog.Handler
}

func NewContextHandler(handler slog.Handler) ContextHandler {
	return ContextHandler{Handler: handler}
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	requestID := middleware.RequestIDFromContext(ctx)
	if requestID != "" {
		r.AddAttrs(slog.String("request_id", requestID))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{h.Handler.WithGroup(name)}
}
