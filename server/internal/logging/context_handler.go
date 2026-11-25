package logging

import (
	"context"
	"log/slog"

	"github.com/jay41chen/danmaku/server/internal/middleware"
)

// ContextHandler 從 ctx 取出 request-scoped 資料（如 request_id）加進每筆 log。
//
// request_id 要留在最上層，所以 WithGroup 不轉給 inner handler：
// 否則 inner 已經開著 group，Handle 裡加的 request_id 會被包進去（{"db":{"sql":..,"request_id":..}}）。
// 做法是 inner 永遠不開 group，開過的 group 和之後的 attrs 記在 chain 裡，Handle 時才包成 slog.Group，
// request_id 最後再加在最上層（做法同官方的 slog handler 撰寫指南）。
type ContextHandler struct {
	inner slog.Handler
	// 第一個 WithGroup 之後的 group / attrs，由外到內；immutable（只會複製後 append，不改共用的底層陣列）
	chain []chainEntry
}

// 一個 entry 是「開一個 group」（group != ""）或「一批 attrs」（group == ""）
type chainEntry struct {
	group string
	attrs []slog.Attr
}

func NewContextHandler(handler slog.Handler) ContextHandler {
	return ContextHandler{inner: handler}
}

func (h ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	// 從最內層往外包：group 裡沒有 attrs 就整個省略（slog 規則）
	for i := len(h.chain) - 1; i >= 0; i-- {
		e := h.chain[i]
		if e.group == "" {
			attrs = append(append([]slog.Attr(nil), e.attrs...), attrs...)
			continue
		}
		if len(attrs) == 0 {
			continue
		}
		attrs = []slog.Attr{{Key: e.group, Value: slog.GroupValue(attrs...)}}
	}

	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	nr.AddAttrs(attrs...)
	if requestID := middleware.RequestIDFromContext(ctx); requestID != "" {
		nr.AddAttrs(slog.String("request_id", requestID))
	}
	return h.inner.Handle(ctx, nr)
}

func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// 還沒開過 group：直接交給 inner，最上層的 attrs（如 component）照舊
	if len(h.chain) == 0 {
		return ContextHandler{inner: h.inner.WithAttrs(attrs)}
	}
	return ContextHandler{inner: h.inner, chain: h.appendEntry(chainEntry{attrs: append([]slog.Attr(nil), attrs...)})}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return ContextHandler{inner: h.inner, chain: h.appendEntry(chainEntry{group: name})}
}

// 複製再 append，不動到跟 receiver 共用的 slice
func (h ContextHandler) appendEntry(e chainEntry) []chainEntry {
	c := make([]chainEntry, len(h.chain), len(h.chain)+1)
	copy(c, h.chain)
	return append(c, e)
}
