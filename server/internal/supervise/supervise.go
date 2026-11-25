// 全站共用的背景 worker（Redis consumer、presence、SUB/UNSUB…）的 panic 保護：接住後原地重啟，太頻繁就讓 process 掛掉
// 只給「可以安全重啟」的 worker 用：狀態存在共用的 struct 裡、重啟後能接著做（reconciliation loop 這類），
// 不能依賴上一個 goroutine 區域變數裡的東西
package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

const (
	// window 內 panic 達到 maxPanics 次就放棄（Q692 A：1 分鐘內第 3 次）
	// 每則訊息都會觸發的 bug 幾毫秒內就連炸 3 次；低頻的 bug（例如每 20 秒一次）也會在 1 分鐘內放棄，不會一直接住、一直丟訊息
	maxPanics = 3
	window    = time.Minute
)

// Run 在目前的 goroutine 上跑 worker，worker panic 時記 log、加 metric，然後馬上重新跑一次。
// 1 分鐘內第 3 次 panic 就不再接（時間從第一次 panic 開始算），重新 panic 讓 process 掛掉，交給部署平台用乾淨的狀態重啟（"let it crash"）。
//
// 會一直卡住，直到 worker 正常 return（例如 ctx 被取消）才 return，所以呼叫端要自己開 goroutine：
//
//	go supervise.Run(ctx, logger, "broadcast", func(ctx context.Context) { ... })
//
// name 會進 log 和 metric 的 label（worker_panics_total{worker}），只能用程式碼裡寫死的字串。
// 重啟是馬上的、不等待：Redis consumer 這類 worker 等越久，排在 buffer 裡的訊息越可能被丟掉。
func Run(ctx context.Context, logger *slog.Logger, name string, worker func(context.Context)) {
	run(ctx, logger, name, worker, time.Now)
}

// 實作細節：Run 的本體，把時間來源抽出來，test 才能模擬「過了 1 分鐘」
func run(ctx context.Context, logger *slog.Logger, name string, worker func(context.Context), now func() time.Time) {
	// 目前這個時間窗：從窗裡第一次 panic 開始算，count = 窗裡 panic 了幾次
	// startTime 用零值（西元 1 年）代表「還沒有窗」：第一次 panic 時 now() 一定距離它超過 window，
	// 所以一定走進「開新窗」那條路，不用另外判斷是不是第一次（TestRunWindowMeasuredFromFirstPanicNotStart）
	var startTime time.Time
	var count int

	for {
		rec, stack := runOnce(ctx, worker)

		if rec == nil {
			return
		}

		logger.ErrorContext(ctx, "worker panic recovered", "worker", name, "panic", rec, "stacktrace", string(stack))
		prom.WorkerPanic(name)

		if now().Sub(startTime) > window {
			count = 1
			startTime = now()
		} else {
			count += 1
		}
		if count >= maxPanics {
			panic(fmt.Sprintf("supervise: worker %q panicked %d times within %v, giving up: %v", name, count, window, rec))
		}
	}
}

// 實作細節：跑一次 worker，panic 時接住，回傳 panic 的值和當下的 stack；沒有 panic 就回傳 (nil, nil)
//
// 回傳值要用具名回傳值（rec、stack）：panic 之後最後那行 return 根本跑不到，唯一還會執行的是 defer，
// 而 defer 裡寫 return 只會結束那個匿名函式。具名回傳值是函式一開始就存在的變數，
// defer 把值寫進去，runOnce 結束時回傳的就是它們當下的值（等同 Kotlin try 是 expression、catch 直接產生回傳值）
// 注意 defer 裡要用 `=`：寫成 `rec := recover()` 會另外宣告一個區域變數蓋住具名回傳值，回傳的永遠是 nil
// debug.Stack() 也要在 defer 裡呼叫：那時還在 panic 的現場，stack 才會指到真正炸掉的那一行（見 go-notes 1.27）
func runOnce(ctx context.Context, worker func(context.Context)) (rec any, stack []byte) {
	defer func() {
		rec = recover()
		switch {
		case rec != nil:
			stack = debug.Stack()
		}
	}()
	worker(ctx)
	return nil, nil
}
