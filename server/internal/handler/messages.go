package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// 廣播（Redis PUBLISH）的上限時間：Redis 正常 < 1ms，500ms 只是卡住時的保險
const publishTimeout = 500 * time.Millisecond

// 限流規則（機制在 ratelimit.go），key 是 user id
// 兩條一起用：burst 擋瞬間洗版，sustain 擋持續慢慢刷（每秒 3 則剛好卡在 burst 下面也會被 sustain 擋）
// 只能擋「單一帳號」：多開帳號的總量擋不住，要靠 W5 的全域限流
var (
	messageBurstRule   = rateRule{"message:burst", 3, time.Second}
	messageSustainRule = rateRule{"message:sustain", 20, 10 * time.Second}
)

type MessageHandler struct {
	db      store.DBStoreInterface
	redis   store.RedisStoreInterface
	limiter store.RateLimiterInterface
	logger  *slog.Logger
}

// constructor
func NewMessageHandler(db store.DBStoreInterface, redis store.RedisStoreInterface, limiter store.RateLimiterInterface) *MessageHandler {
	return &MessageHandler{
		db:      db,
		redis:   redis,
		limiter: limiter,
		logger:  slog.Default().With("component", "messages"),
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

// GET 分兩步：先用 public id 查房間拿內部 id，再查訊息（POST 則是 CreateMessage 內一次做完）
// 因為這個 API 每次進房才打一次；兩步之間房間剛好被刪的 race，
// 只會多回一次剛刪房間的歷史訊息，無害
func (this *MessageHandler) getMessages(w http.ResponseWriter, r *http.Request, roomPublicID string) {
	room, err := this.db.GetRoomByPublicID(r.Context(), roomPublicID)
	if errors.Is(err, store.ErrRoomNotFound) {
		// 已刪除的房間也回 404（跟 HandleGetRoom 一樣）
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get room failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	msgs, err := this.db.QueryMessage(r.Context(), room.ID, 50)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "query messages failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// DB query出來的是新到舊
	slices.Reverse(msgs)
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, msgs)
}

// 要登入（RequireAuth）；訊息存進 path 指定的房間
func (this *MessageHandler) HandleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	this.createMessage(w, r, r.PathValue("public_id"))
}

// flow:
// 取 user id（RequireAuth 放的）
// 檢查 body
// 限流
// 存 DB（房間不存在 → 404、user 不存在 → 401）
// 發到 Redis 廣播
// 回 201
func (this *MessageHandler) createMessage(w http.ResponseWriter, r *http.Request, roomPublicID string) {
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}

	var req createMessageRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// client 也會檢查，但那只是 UX；server 才是防線（curl 可以繞過 client）
	// 只檢查、不改寫：存的是原始 content（不 trim、不正規化）
	if err := validateMessageContent(req.Content); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 在 DB 之前擋，不讓洗版的人讓 server 做白工
	if !this.allow(w, r, userID) {
		return
	}

	msg, err := this.db.CreateMessage(r.Context(), roomPublicID, userID, req.Content)
	if errors.Is(err, store.ErrRoomNotFound) {
		// 房間不存在或已刪除（兩者都不讓外人分辨）
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrMessageUserNotFound) {
		// user 已刪除但 access token 還沒過期：回跟 RequireAuth 一模一樣的 401
		// client 會去 /refresh → refresh token 已經被 CASCADE 刪掉 → 401 → 跳登入
		httpx.WriteUnauthorized(w)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "create message failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// DB 已經存好了：廣播失敗只記 log、仍回 201（回 5xx 的話 client 重送會變成重複的訊息）
	// 沒收到即時廣播的人下次載入歷史訊息時還是看得到
	this.broadcast(r.Context(), roomPublicID, wsEnvelope{Type: wsTypeMessage, Payload: msg})

	// body 是 msg 本身，不是 envelope：envelope 是 WS 的格式
	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, msg)
}

// CreateMessage 維持 request ctx：INSERT 之前 / 之中被取消 → 沒存也沒廣播，結果一致
// broadcast 不跟著 request 取消：DB 已經 commit（副作用已發生），廣播是在完成這件事，
// 不該因為發起的人走了就停，否則房間內的觀眾要到重連才看得到這則
// WithoutCancel 會連 deadline 一起拿掉 → 自己加 publishTimeout，Redis 卡住時不會無限等
// （client 在等 201，最多多等 500ms）；WithoutCancel 保留 ctx 的 value（request_id 等），log 仍串得起來
func (this *MessageHandler) broadcast(ctx context.Context, roomPublicID string, env wsEnvelope) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	data, err := json.Marshal(env)
	if err != nil {
		this.logger.ErrorContext(ctx, "marshal ws envelope failed", "error", err)
		return
	}
	if err := this.redis.Publish(ctx, roomPublicID, string(data)); err != nil {
		this.logger.ErrorContext(ctx, "publish to redis failed", "error", err)
	}
}

// 兩條規則都要問（不是第一條擋住就停）：被擋時 Retry-After 要給比較長的那個，
// 只回短的 client 馬上重試又會被另一條擋
// 回傳 true = 可以繼續；false = 已經回了 429，handler 要直接 return
func (this *MessageHandler) allow(w http.ResponseWriter, r *http.Request, userID int64) bool {
	value := strconv.FormatInt(userID, 10)
	burstOK, burstRetry := checkRate(r, this.limiter, this.logger, messageBurstRule, value)
	sustainOK, sustainRetry := checkRate(r, this.limiter, this.logger, messageSustainRule, value)
	if burstOK && sustainOK {
		return true
	}
	writeTooManyRequests(w, max(burstRetry, sustainRetry))
	return false
}
