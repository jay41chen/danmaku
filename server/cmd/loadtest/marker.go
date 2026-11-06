package main

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 彈幕內容裡的 marker：lt-<run>-<step>-<seq>。sampled 連線收到後靠它對回「哪個 step 的第幾筆」，
// 再用排程時間算 send-to-receive 延遲。只用英數與 -，過 server 的內容檢查（PRECIS）沒問題，長度遠小於 100 字元

// 組出某一筆的內容
func makeMarker(run string, step, seq int) string {
	return "lt-" + run + "-" + strconv.Itoa(step) + "-" + strconv.Itoa(seq)
}

// 解析內容：不是這次 run 的 marker 回 ok=false
func parseMarker(run, content string) (step, seq int, ok bool) {
	rest, found := strings.CutPrefix(content, "lt-"+run+"-")
	if !found {
		return 0, 0, false
	}
	stepStr, seqStr, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	step, err1 := strconv.Atoi(stepStr)
	seq, err2 := strconv.Atoi(seqStr)
	if err1 != nil || err2 != nil || step < 0 || seq < 0 {
		return 0, 0, false
	}
	return step, seq, true
}

// 一個 step 期間 sampled 連線的收件紀錄：recv[sample][seq] = 收到時間相對 Start 的位移 + 1（0 = 沒收到）
// 每條 sampled 連線只由自己的 reader goroutine 寫自己的那一列；用 atomic 是為了結束時主 goroutine 讀取不用額外同步
type stepRecorder struct {
	step  int
	start time.Time
	recv  [][]atomic.Int64
}

func newStepRecorder(step int, start time.Time, samples, total int) *stepRecorder {
	recv := make([][]atomic.Int64, samples)
	for i := range recv {
		recv[i] = make([]atomic.Int64, total)
	}
	return &stepRecorder{step: step, start: start, recv: recv}
}

// 記下 sample 號連線在 at 時收到第 seq 筆；重複收到只留第一次
func (r *stepRecorder) record(sample, seq int, at time.Time) {
	if sample < 0 || sample >= len(r.recv) || seq < 0 || seq >= len(r.recv[sample]) {
		return
	}
	r.recv[sample][seq].CompareAndSwap(0, int64(at.Sub(r.start))+1)
}

// 取出某連線某筆的收到位移；沒收到回 ok=false
func (r *stepRecorder) received(sample, seq int) (offset time.Duration, ok bool) {
	v := r.recv[sample][seq].Load()
	if v == 0 {
		return 0, false
	}
	return time.Duration(v - 1), true
}
