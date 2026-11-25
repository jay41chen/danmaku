package supervise

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/prometheus/promtest"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// 假的 worker：第 i 次被呼叫時，先把時鐘設成 at[i]（相對 base），再 panic；呼叫次數超過 len(at) 就正常 return
// 用來模擬「每次 panic 發生在什麼時間」
type scriptedWorker struct {
	base  time.Time
	at    []time.Duration
	clock time.Time
	calls int
}

func newScriptedWorker(at ...time.Duration) *scriptedWorker {
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	return &scriptedWorker{base: base, at: at, clock: base}
}

func (w *scriptedWorker) now() time.Time { return w.clock }

func (w *scriptedWorker) run(ctx context.Context) {
	i := w.calls
	w.calls++
	if i >= len(w.at) {
		return
	}
	w.clock = w.base.Add(w.at[i])
	panic("injected by test")
}

// 跑 run，回傳它最後有沒有重新 panic（放棄）
func runAndCatch(t *testing.T, name string, w *scriptedWorker) (gaveUp bool) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			gaveUp = true
		}
	}()
	run(context.Background(), discardLogger, name, w.run, w.now)
	return false
}

func TestRunReturnsWhenWorkerReturns(t *testing.T) {
	const name = "test_returns"
	w := newScriptedWorker() // 第一次就正常 return
	before := promtest.WorkerPanicCount(name)

	if runAndCatch(t, name, w) {
		t.Fatal("run panicked, want normal return")
	}
	if w.calls != 1 {
		t.Errorf("worker calls = %d, want 1", w.calls)
	}
	if got := promtest.WorkerPanicCount(name) - before; got != 0 {
		t.Errorf("worker_panics_total += %v, want 0 (no panic happened)", got)
	}
}

// 偶發 panic：接住、記 metric、馬上重跑
func TestRunRestartsAfterPanic(t *testing.T) {
	const name = "test_restarts"
	w := newScriptedWorker(0, time.Second) // 2 次 panic，第 3 次正常 return
	before := promtest.WorkerPanicCount(name)

	if runAndCatch(t, name, w) {
		t.Fatal("run gave up after 2 panics, want restart")
	}
	if w.calls != 3 {
		t.Errorf("worker calls = %d, want 3", w.calls)
	}
	if got := promtest.WorkerPanicCount(name) - before; got != 2 {
		t.Errorf("worker_panics_total += %v, want 2", got)
	}
}

// 同一分鐘內第 3 次 panic 就放棄（重新 panic），不會再跑第 4 次
func TestRunGivesUpOnThirdPanicWithinWindow(t *testing.T) {
	const name = "test_gives_up"
	w := newScriptedWorker(0, time.Millisecond, 2*time.Millisecond, 3*time.Millisecond)
	before := promtest.WorkerPanicCount(name)

	if !runAndCatch(t, name, w) {
		t.Fatal("run returned normally, want it to give up")
	}
	if w.calls != 3 {
		t.Errorf("worker calls = %d, want 3 (give up on the 3rd panic)", w.calls)
	}
	// 放棄的那次也要記到
	if got := promtest.WorkerPanicCount(name) - before; got != 3 {
		t.Errorf("worker_panics_total += %v, want 3", got)
	}
}

// 時間窗從第一次 panic 開始算，不是從 run 啟動：跑了 3 小時才連炸 3 次，一樣第 3 次放棄
func TestRunWindowStartsAtFirstPanic(t *testing.T) {
	const name = "test_window_start"
	w := newScriptedWorker(3*time.Hour, 3*time.Hour+time.Millisecond, 3*time.Hour+2*time.Millisecond, 3*time.Hour+3*time.Millisecond)

	if !runAndCatch(t, name, w) {
		t.Fatal("run returned normally, want it to give up")
	}
	if w.calls != 3 {
		t.Errorf("worker calls = %d, want 3", w.calls)
	}
}

// 窗從第一次 panic 開始：50s、70s、80s 三次只跨 30 秒，算同一個窗 → 第 3 次放棄
// （如果窗從 run 啟動開始算，70s 會被當成新的窗，要到第 4 次才放棄）
func TestRunWindowMeasuredFromFirstPanicNotStart(t *testing.T) {
	const name = "test_window_from_first"
	w := newScriptedWorker(50*time.Second, 70*time.Second, 80*time.Second, 81*time.Second)

	if !runAndCatch(t, name, w) {
		t.Fatal("run returned normally, want it to give up")
	}
	if w.calls != 3 {
		t.Errorf("worker calls = %d, want 3", w.calls)
	}
}

// 距離窗的開始超過 1 分鐘：重新計數。0s、30s 兩次之後，第 3 次在 90s → 新的窗（算 1），不放棄
func TestRunResetsAfterWindow(t *testing.T) {
	const name = "test_window_reset"
	w := newScriptedWorker(0, 30*time.Second, 90*time.Second) // 3 次 panic，第 4 次正常 return

	if runAndCatch(t, name, w) {
		t.Fatal("run gave up, want the window to reset at 90s")
	}
	if w.calls != 4 {
		t.Errorf("worker calls = %d, want 4", w.calls)
	}
}
