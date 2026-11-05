package redisstore

import (
	"context"
	"log/slog"
	"time"
)

// PubSub 聾掉偵測：連線沒斷（go-redis 不會重連）、但再也收不到任何訊息 —— 所有房間靜靜地漏訊息，沒有任何錯誤
// 做法：定期對自己的心跳 channel（heartbeatChannel）PUBLISH，再看 consumer 有沒有收到；連續收不到就重建 PubSub（rebuildPubSub）
const (
	// 發心跳的間隔：聾掉後 30～45 秒才會被發現（剛收到心跳就聾掉時，要多等一輪才開始計 miss）
	heartbeatInterval = 15 * time.Second
	// 連續幾輪沒收到心跳才判定聾掉：1 次太敏感（一次網路抖動就重建），2 次容得下單次遺失
	heartbeatMissLimit = 2
)

type pubsubWatchdog struct {
	publish func(ctx context.Context) error // 發一則心跳到自己的 heartbeat channel
	rebuild func(ctx context.Context)       // RedisStore.rebuildPubSub
	// consumer 收到心跳時 beat() 送進來；buffer 1，run 每輪取出看「上一輪之後有沒有收到」
	received chan struct{}
	logger   *slog.Logger
}

func newPubsubWatchdog(
	publish func(ctx context.Context) error,
	rebuild func(ctx context.Context),
	logger *slog.Logger,
) *pubsubWatchdog {
	return &pubsubWatchdog{
		publish:  publish,
		rebuild:  rebuild,
		received: make(chan struct{}, 1),
		logger:   logger,
	}
}

// consumer 收到心跳時呼叫：非阻塞送（跟 markDirtyLocked 一樣）
// 已經有一個訊號在 buffer 裡就不用再送；阻塞的話會卡住整個 Subscribe 的消費迴圈，所有房間的訊息跟著停
func (this *pubsubWatchdog) beat() {
	select {
	case this.received <- struct{}{}:
	default:
	}
}

// 定期檢查 PubSub 連線還收不收得到訊息，連續收不到就重建；生命週期跟著 application server
// 用途：偵測連線沒斷、但已經收不到任何訊息的情況（half-open）
func (this *pubsubWatchdog) run(ctx context.Context, tick <-chan time.Time) {
	missCount := 0
	published := false
	for {
		select {
		// 每次收到tick都要檢查
		case <-tick:
			got := false
			select {
			case <-this.received:
				got = true
			default:
			}
			// 如果沒收到got的話要看看是不是前一次publish失敗
			// 如果前一次publish就失敗的話，跳過這一次
			if got {
				missCount = 0
			} else if published {
				missCount++
			}
			if missCount >= heartbeatMissLimit {
				// 多次都沒收到，判定死亡
				// 重新建立再開始跑
				this.rebuild(ctx)
				missCount = 0
			}
			// 開始發送下一輪
			err := this.publish(ctx)
			published = err == nil
			if err != nil {
				this.logger.WarnContext(ctx, "publish heartbeat failed", "error", err)
			}

		case <-ctx.Done():
			return
		}
	}
}
