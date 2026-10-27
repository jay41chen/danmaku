package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// 訊息長度上限（字元數，不是 bytes：中文一個字 3 bytes）
const maxMessageRunes = 100

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

// POST /messages 的 request body
type createMessageRequest struct {
	Content string `json:"content"`
}

func (this *MessageHandler) HandleGetMessages(w http.ResponseWriter, r *http.Request) {
	httpx.SetContentType(w)

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

// flow:
// 取 user id（RequireAuth 放的）
// 檢查 body
// 限流
// 存 DB
// 發到 Redis 廣播
// 回 201
func (this *MessageHandler) HandleCreateMessage(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		// 走到這裡是 server 設定錯誤、不是 client 的錯：這個 route 忘了用 post 註冊（誤用 publicPost）
		this.logger.ErrorContext(r.Context(), "no user id in context, route must be registered with post, not publicPost")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var req createMessageRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// client 也會檢查，但那只是 UX；server 才是防線（curl 可以繞過 client）
	// 存的是原始 content，不 trim：只用 trim 判斷「是不是整串空白」
	// 已知問題：零寬空白（U+200B 等）不算 space，擋不到，見 ROADMAP W4b
	if strings.TrimSpace(req.Content) == "" {
		http.Error(w, "content must not be empty", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(req.Content) > maxMessageRunes {
		http.Error(w, "content too long", http.StatusBadRequest)
		return
	}

	// 在 DB 之前擋，不讓洗版的人讓 server 做白工
	if !this.allow(w, r, userID) {
		return
	}

	msg, err := this.db.CreateMessage(r.Context(), userID, req.Content)
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
	this.broadcast(r.Context(), wsEnvelope{Type: wsTypeMessage, Payload: msg})

	httpx.SetContentType(w)
	w.WriteHeader(http.StatusCreated)
	// body 是 msg 本身，不是 envelope：envelope 是 WS 的格式
	json.NewEncoder(w).Encode(msg)
}

func (this *MessageHandler) broadcast(ctx context.Context, env wsEnvelope) {
	data, err := json.Marshal(env)
	if err != nil {
		this.logger.ErrorContext(ctx, "marshal ws envelope failed", "error", err)
		return
	}
	if err := this.redis.Publish(ctx, string(data)); err != nil {
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
