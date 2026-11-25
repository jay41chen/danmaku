package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/ratelimit"
	"github.com/jay41chen/danmaku/server/internal/textrule"
)

// GET 歷史訊息回幾則（最新的幾則，新到舊查出來再反轉成舊到新）
// 重連後 client 靠這支補撈斷線期間的洞：100 則在熱門房間（約 50 則/秒）約涵蓋 2 秒
// 不用 200：回應會到約 23 KB，本機實測 2,000 GET/s 時 p99 從 5ms 變 227ms
// 匯出：這是 use case 擁有的規則，其他 package（例如 handler 測試）讀它，不自己再寫一份
const HistoryLimit = 100

// 限流規則（機制在 internal/ratelimit），key 是 user id
// 兩條一起用：burst 擋瞬間洗版，sustain 擋持續慢慢刷（每秒 3 則剛好卡在 burst 下面也會被 sustain 擋）
// 只能擋「單一帳號」：多開帳號的總量擋不住，要靠 W5 的全域限流
// 匯出：同 HistoryLimit，規則由 use case 擁有，其他 package 只讀
var (
	MessageBurstRule   = ratelimit.Rule{Name: "message:burst", Limit: 3, Window: time.Second}
	MessageSustainRule = ratelimit.Rule{Name: "message:sustain", Limit: 20, Window: 10 * time.Second}
)

// 這個 use case 需要的 DB 方法（consumer-side interface，同 ADR 0009）
type messageStore interface {
	CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (msg model.Message, banned bool, err error)
	QueryMessage(ctx context.Context, roomID int64, limit int) ([]model.Message, error)
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)
}

// 把新訊息廣播給房間裡的連線；訊息格式和發送失敗的處理都是實作方的事，所以沒有回傳 error
type messageNotifier interface {
	NotifyMessage(ctx context.Context, roomPublicID string, msg model.Message)
}

// 限流器：只需要 Allow
type messageLimiter interface {
	ratelimit.Limiter
}

// 彈幕訊息的業務操作：不知道 HTTP、SQL 和 WS 訊息格式
type MessageUseCase struct {
	store    messageStore
	notifier messageNotifier
	limiter  messageLimiter
	logger   *slog.Logger
}

// constructor
func NewMessageUseCase(store messageStore, notifier messageNotifier, limiter messageLimiter) *MessageUseCase {
	return &MessageUseCase{
		store:    store,
		notifier: notifier,
		limiter:  limiter,
		logger:   slog.Default().With("component", "messages"),
	}
}

// 發一則訊息：檢查內容 → 限流 → 存 DB → 廣播
// 內容不合法回 *model.CodedError；被限流回 *model.RateLimitedError；限流查不到結果且規則是 fail closed 回 model.ErrRateLimiterUnavailable；其他 store 的 error 原樣回傳
// 用途：POST /rooms/{public_id}/messages
func (u *MessageUseCase) Post(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, error) {
	// client 也會檢查，但那只是 UX；server 才是防線（curl 可以繞過 client）
	// 只檢查、不改寫：存的是原始 content（不 trim、不正規化）
	if err := textrule.MessageContent(content); err != nil {
		return model.Message{}, err
	}

	// 在 DB 之前擋，不讓洗版的人讓 server 做白工
	// 兩條規則都要問（不是第一條擋住就停）：被擋時 Retry-After 要給比較長的那個，
	// 只回短的 client 馬上重試又會被另一條擋
	value := strconv.FormatInt(userID, 10)
	burstOK, burstRetry, burstErr := ratelimit.Check(ctx, u.limiter, u.logger, MessageBurstRule, value)
	sustainOK, sustainRetry, sustainErr := ratelimit.Check(ctx, u.limiter, u.logger, MessageSustainRule, value)
	// 只有 FailClosed 的規則會回 err（現在訊息的規則都是 fail open，所以不會發生）；
	// 原樣往上回，handler 翻成 503，不要被當成 429
	if burstErr != nil {
		return model.Message{}, burstErr
	}
	if sustainErr != nil {
		return model.Message{}, sustainErr
	}
	if !burstOK || !sustainOK {
		return model.Message{}, &model.RateLimitedError{RetryAfter: max(burstRetry, sustainRetry)}
	}

	msg, banned, err := u.store.CreateMessage(ctx, roomPublicID, userID, content)
	if err != nil {
		return model.Message{}, err
	}

	// DB 已經存好了：廣播失敗只記 log、仍回成功（回 5xx 的話 client 重送會變成重複的訊息）
	// 沒收到即時廣播的人下次載入歷史訊息時還是看得到
	// 被 ban（shadow ban）的作者：訊息已存，照常回成功，但不廣播，別人看不到；對他自己看不出任何差別
	if !banned {
		u.notifier.NotifyMessage(ctx, roomPublicID, msg)
	}
	return msg, nil
}

// 讀房間的歷史訊息（舊到新，最多 HistoryLimit 則）；公開，不用登入
// error 帶步驟名稱（get room / query messages），房間不存在時 errors.Is(err, model.ErrRoomNotFound) 仍成立
// 用途：GET /rooms/{public_id}/messages
func (u *MessageUseCase) History(ctx context.Context, roomPublicID string) ([]model.Message, error) {
	// GET 分兩步：先用 public id 查房間拿內部 id，再查訊息（POST 則是 CreateMessage 內一次做完）
	// 因為這個 API 每次進房才打一次；兩步之間房間剛好被刪的 race，
	// 只會多回一次剛刪房間的歷史訊息，無害
	room, err := u.store.GetRoomByPublicID(ctx, roomPublicID)
	if err != nil {
		return nil, fmt.Errorf("get room: %w", err)
	}
	msgs, err := u.store.QueryMessage(ctx, room.ID, HistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	// DB query出來的是新到舊
	slices.Reverse(msgs)
	return msgs, nil
}
