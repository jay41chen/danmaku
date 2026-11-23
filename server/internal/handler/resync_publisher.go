package handler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// 延遲補發 resync 的 publisher decorator：包住 messagePublisher，本身也實作 messagePublisher
// 情境：某間房的 PUBLISH 失敗（例如這台 server 連不到 Redis），訊息已經進 Postgres、POST 回 201，
// 但沒有人從 WS 收到，其他 server 的 viewer 也不知道自己漏了一則
// 做法：失敗時在這台 server 的記憶體記下「這間房漏發過」，下一次對該房 PUBLISH 成功時，
// 多發一個 resync frame 到同一個 Redis room channel；所有 server 的 viewer 都會收到並重抓歷史
// 為什麼記在各 server 的記憶體：只有 PUBLISH 失敗的那台知道自己漏了；失敗當下 Redis 多半連不到，記在 Redis 也會失敗；
// 多台 server 各自記、各自補發，viewer 可能收到重複的 resync，這是接受的 false positive（同 ADR 0018 的取捨）
// 已知限制：server 掛掉會忘記 pending；安靜的房間要等下一次 PUBLISH 成功才會補發；
// 已刪除房間的 entry 會留在 map 裡（數量有上限，接受）
type ResyncPublisher struct {
	inner  messagePublisher
	logger *slog.Logger

	mu      sync.Mutex
	pending map[string]time.Time // roomID → 第一次失敗、還沒被 resync 涵蓋的時間
}

// constructor
func NewResyncPublisher(inner messagePublisher) *ResyncPublisher {
	return &ResyncPublisher{
		inner:   inner,
		logger:  slog.Default().With("component", "resync_publisher"),
		pending: make(map[string]time.Time),
	}
}

// 轉發 Publish；失敗時記下這間房，下一次成功時補發一個 resync
// 失敗的錯誤原樣回傳：呼叫端 publishEnvelope 已經 log + 計 publish_failed，這裡不重複
// 先「取走」flag 再發 resync（失敗再放回去）：同一間房同一台 server 最多一個 resync 在途，
// 恢復瞬間不會因為大量並發 publish 讓重抓歷史的 GET 暴增；也不用在網路 I/O 期間持有 lock
func (p *ResyncPublisher) Publish(ctx context.Context, roomID, msg string) error {
	err := p.inner.Publish(ctx, roomID, msg)
	if err != nil {
		p.mu.Lock()
		if _, ok := p.pending[roomID]; !ok {
			p.pending[roomID] = time.Now() // 已有 entry 就保留較早的時間
		}
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	since, ok := p.pending[roomID]
	if ok {
		delete(p.pending, roomID)
	}
	p.mu.Unlock()
	if !ok {
		return nil
	}

	frame := encodeResync(roomID)
	if frame == nil {
		// encodeResync 是固定 struct 的 marshal，實務上不會回 nil；但不該安靜地失敗
		p.putBack(roomID, since)
		p.logger.ErrorContext(ctx, "deferred resync encode failed", "room", roomID)
		return nil
	}
	// 沿用同一個 ctx（帶著 publishEnvelope 的 publishTimeout）；額度用完就失敗，等下一次 publish 再補
	if rerr := p.inner.Publish(ctx, roomID, string(frame)); rerr != nil {
		p.putBack(roomID, since)
		p.logger.ErrorContext(ctx, "deferred resync publish failed", "room", roomID, "error", rerr)
		return nil // 原本那則訊息已經發成功，resync 失敗不影響回傳值，也不計 publish_failed（那是訊息廣播專用）
	}
	prom.DeferredResyncPublished()
	p.logger.InfoContext(ctx, "deferred resync published", "room", roomID, "pending_for", time.Since(since))
	return nil
}

// resync 發送失敗時把 flag 放回去；merge 規則：保留較早的時間
// 放回去之前若別的 goroutine 又失敗而記了新的時間，取較早的那個
// 若別的 goroutine 在空窗期已經成功發過 resync，這裡放回去會多發一次，屬於接受的 false positive
func (p *ResyncPublisher) putBack(roomID string, since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.pending[roomID]; !ok || since.Before(cur) {
		p.pending[roomID] = since
	}
}
