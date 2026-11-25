// 全站共用的背景 worker（Redis consumer、presence、SUB/UNSUB…）的 panic 保護，依「能不能安全重啟」選策略：
//   - Restart：狀態全部在 goroutine 外面（共用的 struct、Redis），panic 後原地重啟，1 分鐘內第 3 次就讓 process 掛掉
//   - Escalate：有狀態只存在 goroutine 裡（區域變數、正在消費的連線），第一次 panic 就讓 process 掛掉
//
// 兩種都會在放棄前記 log（帶 stacktrace）和 worker_panics_total{worker}；放棄 = 重新 panic，交給部署平台重啟
package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

// Restart / Escalate 共用的時間窗：從窗裡第一次 panic 開始算，窗內第幾次 panic 就放棄由各自的門檻決定
const window = time.Minute

// RestartOptions 調整 Restart 的行為，不傳就用預設值（Go 沒有參數預設值，用 functional options 代替）
type RestartOptions func(*restartOption)

// 實作細節：Restart 套用完所有 option 之後的設定
type restartOption struct {
	maxPanics int // window 內第幾次 panic 就放棄（傳給 run 的 maxPanics）
}

// WithMaxPanics 設定 Restart 在 1 分鐘內第 n 次 panic 時放棄（讓 process 掛掉）；預設 3。
// 注意：n 是「第幾次 panic 放棄」，中間重啟了 n-1 次。n <= 0 視為沒設，沿用預設。
func WithMaxPanics(n int) RestartOptions {
	return func(ro *restartOption) {
		if n <= 0 {
			return
		}
		ro.maxPanics = n
	}
}

// Escalate 在目前的 goroutine 上跑 worker，worker 第一次 panic 就記 log、加 metric，然後重新 panic 讓 process 掛掉，
// 交給部署平台用乾淨的狀態重啟。不會原地重啟。
//
// 給「不能安全重啟」的 worker 用：狀態有一部分只存在 goroutine 裡（區域變數、正在消費的連線），
// 重啟後的新 goroutine 接不上，會留下一台活著、/health 也正常，但功能壞掉的 server。直接掛掉比較容易被發現、也會自己恢復。
// 跟直接用 go 開的差別：掛掉之前先經過 slog（帶 stacktrace）和 worker_panics_total，log 和 alert 才看得到。
//
// 會一直卡住，直到 worker 正常 return 才 return，所以呼叫端要自己開 goroutine：
//
//	go supervise.Escalate(ctx, logger, "broadcast", func(ctx context.Context) { ... })
//
// name 會進 log 和 metric 的 label，只能用程式碼裡寫死的字串。
func Escalate(ctx context.Context, logger *slog.Logger, name string, worker func(context.Context)) {
	run(ctx, logger, name, worker, time.Now, 1)
}

// Restart 的放棄門檻：window 內第 3 次 panic 就放棄
// 每則訊息都會觸發的 bug 幾毫秒內就連炸 3 次；低頻的 bug（例如每 20 秒一次）也會在 1 分鐘內放棄，不會一直接住、一直丟訊息
const restartMaxPanics = 3

// Restart 在目前的 goroutine 上跑 worker，worker panic 時記 log、加 metric，然後馬上重新跑一次。
// 1 分鐘內第 3 次 panic 就不再接（時間從第一次 panic 開始算），重新 panic 讓 process 掛掉，交給部署平台用乾淨的狀態重啟（"let it crash"）。
//
// 只給「可以安全重啟」的 worker 用：狀態全部存在 goroutine 外面（共用的 struct、Redis），新的 goroutine 能接著做。
// 有任何狀態只存在 goroutine 裡就用 Escalate。
//
// 會一直卡住，直到 worker 正常 return（例如 ctx 被取消）才 return，所以呼叫端要自己開 goroutine：
//
//	go supervise.Restart(ctx, logger, "presence", func(ctx context.Context) { ... })
//
// name 會進 log 和 metric 的 label（worker_panics_total{worker}），只能用程式碼裡寫死的字串。
// 重啟是馬上的、不等待：等越久，worker 來不及處理的工作（例如 buffer 裡的訊息）越可能被丟掉。
func Restart(ctx context.Context, logger *slog.Logger, name string, worker func(context.Context), opts ...RestartOptions) {
	o := restartOption{maxPanics: restartMaxPanics} // 1. 先放預設值（就是 Kotlin 參數上的 = 3）
	for _, opt := range opts {                      // 2. 呼叫端傳了幾個 option，就一個一個套用
		opt(&o) //    每個 option 都是「改 struct 的函式」，傳指標進去才改得到
	}
	run(ctx, logger, name, worker, time.Now, o.maxPanics) // 3. 用套用完的值
}

// 實作細節：Restart / Escalate 共用的本體，差別只在 maxPanics（時間窗內第幾次 panic 就放棄；Escalate 傳 1 = 第一次就放棄）
// 時間來源抽出來，test 才能模擬「過了 1 分鐘」
func run(ctx context.Context, logger *slog.Logger, name string, worker func(context.Context), now func() time.Time, maxPanics int) {
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

		t := now()
		if t.Sub(startTime) > window {
			count = 1
			startTime = t
		} else {
			count += 1
		}
		if count >= maxPanics {
			// 訊息分開寫：查 log 時一眼看出是 Escalate（不能重啟、第一次就掛）還是 Restart 重啟太多次
			if maxPanics == 1 {
				panic(fmt.Sprintf("supervise: worker %q escalated after a panic (not restartable): %v", name, rec))
			}
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
// debug.Stack() 也要在 defer 裡呼叫：那時還在 panic 的現場，stack 才會指到真正炸掉的那一行
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
