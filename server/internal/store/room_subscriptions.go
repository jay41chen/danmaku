package store

import (
	"context"
	"log/slog"
	"sync"
)

// 這台 server 上每個房間有幾條 WS 連線，決定要不要對 Redis SUBSCRIBE / UNSUBSCRIBE（lazy subscribe）
//   - viewers 0→1：SUBSCRIBE room:{public_id}
//   - viewers 1→0：刪 entry + UNSUBSCRIBE
//
// 只用一把鎖，「改人數」和「送指令」包在一起（否則 X 1→0、Y 0→1 SUBSCRIBE、X 才 UNSUBSCRIBE → 有人在但沒訂）
// 不用 per-room lock：整台 server 只有一個 go-redis PubSub，它的 Subscribe / Unsubscribe
// 本來就在內部的 c.mu 排隊，per-room lock 換不到平行度（見 go-notes 3.29）
// 只算這台 server 自己身上的連線，不需要知道別台
type roomSubscriptions struct {
	mu      sync.Mutex
	viewers map[string]int // room public_id → 這台 server 上在這個房間的連線數；降到 0 就刪掉（不留 entry）

	// 注入：正式環境是 PubSub.Subscribe / Unsubscribe，test 換成假的
	subscribe   func(ctx context.Context, channel string) error
	unsubscribe func(ctx context.Context, channel string) error
	logger      *slog.Logger
}

func newRoomSubscriptions(
	subscribe func(ctx context.Context, channel string) error,
	unsubscribe func(ctx context.Context, channel string) error,
	logger *slog.Logger,
) *roomSubscriptions {
	return &roomSubscriptions{
		viewers:     make(map[string]int),
		subscribe:   subscribe,
		unsubscribe: unsubscribe,
		logger:      logger,
	}
}

// Redis channel 名稱：publish 和 subscribe 兩邊都要用這個，不要各自拼字串
func roomChannel(roomID string) string {
	return "room:" + roomID
}

func (this *roomSubscriptions) Join(ctx context.Context, roomID string) error {
	this.mu.Lock()
	defer this.mu.Unlock()
	n, ok := this.viewers[roomID]
	if !ok {
		// roomID目前沒人，要+1同時subscribe
		err := this.subscribe(ctx, roomChannel(roomID))
		if err != nil {
			// 要記得unsubscribe
			if unsubscribeErr := this.unsubscribe(ctx, roomChannel(roomID)); unsubscribeErr != nil {
				this.logger.WarnContext(ctx, "unsubscribe after failed subscribe failed", "room", roomID, "error", unsubscribeErr)
			}
			return err
		}
		this.viewers[roomID] = 1
	} else {
		this.viewers[roomID] = n + 1
	}
	return nil
}

func (this *roomSubscriptions) Leave(ctx context.Context, roomID string) {
	this.mu.Lock()
	defer this.mu.Unlock()
	n, ok := this.viewers[roomID]
	if !ok {
		// 找不到這個room
		this.logger.WarnContext(ctx, "leave room without join", "room", roomID)
		return
	}
	if n > 1 {
		// 超過一個人，所以不用unsubscribe
		this.viewers[roomID] = n - 1
		return
	}
	err := this.unsubscribe(ctx, roomChannel(roomID))
	if err != nil {
		this.logger.ErrorContext(ctx, "unsubscribe room failed", "room", roomID, "error", err)
	}
	delete(this.viewers, roomID)
}
