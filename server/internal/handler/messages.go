package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 廣播（Redis PUBLISH）的上限時間：Redis 正常 < 1ms，500ms 只是卡住時的保險
const publishTimeout = 500 * time.Millisecond

// 這個 handler 需要的 Redis 方法（為什麼定義在這裡見 authStore）
type messagePublisher interface {
	Publish(ctx context.Context, roomID, msg string) error // 發到 roomChannel(roomID)
}

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 authStore）
type messageUseCase interface {
	Post(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, error)
	History(ctx context.Context, roomPublicID string) ([]model.Message, error)
}

// 訊息 handler：只處理 HTTP，業務規則在 use case
type MessageHandler struct {
	messages messageUseCase
	logger   *slog.Logger
}

// constructor
func NewMessageHandler(messages messageUseCase) *MessageHandler {
	return &MessageHandler{
		messages: messages,
		logger:   slog.Default().With("component", "messages"),
	}
}

// POST /rooms/{public_id}/messages 的 request body
type createMessageRequest struct {
	Content string `json:"content"`
}

// 公開：讀歷史訊息不用登入，知道 public_id 的人都能讀
func (this *MessageHandler) HandleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	this.getMessages(w, r, r.PathValue("public_id"))
}

// 讀歷史訊息；use case 回的 error 翻成 404 / 500
func (this *MessageHandler) getMessages(w http.ResponseWriter, r *http.Request, roomPublicID string) {
	msgs, err := this.messages.History(r.Context(), roomPublicID)
	if errors.Is(err, model.ErrRoomNotFound) {
		// 已刪除的房間也回 404（跟 HandleGetRoom 一樣）
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get messages failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// 只有 200 才加（404 / 500 不加，不然錯誤回應會被 CDN 快取）
	// s-maxage 只對 CDN / 共用快取有作用，對瀏覽器沒有作用；TTL 1 秒是為了讓重連補撈拿得到剛發生的洞（同一秒內大量重連的人共用一份回應，不會一起打到 DB）
	// 上限：快照最多舊 1 秒，所以接上 CDN 後，每次補撈最多可能漏最後約 1 秒；到時再決定補撈要不要繞過快取
	// 現在 prod 前面沒有 CDN，這行沒有作用也沒有風險
	// 之後前面放 CDN 或 nginx 時：要避免快取到期瞬間全部回源，要開 stale-while-updating 類設定
	// （實測 nginx 只開 proxy_cache_lock 時，EXPIRED 的 1000 個併發全部回源；加上 proxy_cache_use_stale updating 後最多 1 個）
	w.Header().Set("Cache-Control", "public, s-maxage=1")
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, msgs)
}

// 要登入（RequireAuth）；訊息存進 path 指定的房間
func (this *MessageHandler) HandleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	this.createMessage(w, r, r.PathValue("public_id"))
}

// flow:
// 取 user id（RequireAuth 放的）
// 解析 body
// 交給 use case（檢查內容、限流、存 DB、廣播）
// 回 201
func (this *MessageHandler) createMessage(w http.ResponseWriter, r *http.Request, roomPublicID string) {
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}

	var req createMessageRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	msg, err := this.messages.Post(r.Context(), roomPublicID, userID, req.Content)
	var coded *model.CodedError
	var limited *model.RateLimitedError
	switch {
	case errors.As(err, &coded):
		httpx.WriteCodedError(w, r, this.logger, http.StatusBadRequest, coded)
		return
	case errors.As(err, &limited):
		writeTooManyRequests(w, limited.RetryAfter)
		return
	case errors.Is(err, model.ErrRoomNotFound):
		// 房間不存在或已刪除（兩者都不讓外人分辨）
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	case errors.Is(err, model.ErrMessageUserNotFound):
		// user 已刪除但 access token 還沒過期：回跟 RequireAuth 一模一樣的 401
		// client 會去 /refresh → refresh token 已經被 CASCADE 刪掉 → 401 → 跳登入
		httpx.WriteUnauthorized(w)
		return
	case err != nil:
		this.logger.ErrorContext(r.Context(), "create message failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// body 是 msg 本身，不是 envelope：envelope 是 WS 的格式
	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, msg)
}

// 把 envelope 編碼後發到房間的 Redis channel；失敗只記 log 並計數、不回傳 error（呼叫端的 DB 已經 commit，沒有補救的辦法）
// 抽成共用函式：訊息廣播和 user_hidden 廣播走同一條路，timeout / 失敗計數的規則只有一份
func publishEnvelope(ctx context.Context, logger *slog.Logger, pub messagePublisher, roomPublicID string, env wsEnvelope) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	data, err := json.Marshal(env)
	if err != nil {
		logger.ErrorContext(ctx, "marshal ws envelope failed", "error", err)
		return
	}
	if err := pub.Publish(ctx, roomPublicID, string(data)); err != nil {
		prom.PublishFailedTotal.Inc()
		logger.ErrorContext(ctx, "publish to redis failed", "error", err)
	}
}
