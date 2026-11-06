package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/jay41chen/danmaku/server/internal/loadtiming"
)

// 讀 server 的 LOADTEST_TIMING_LOG 輸出（log 檔裡 msg 是 loadtest_timing 的行），依時間對到每個 step 的量測窗口

type timingRecord struct {
	Time time.Time       `json:"time"`
	Msg  string          `json:"msg"`
	Data loadtiming.Line `json:"data"`
}

var timingNeedle = []byte(`"` + loadtiming.LogMessage + `"`)

// 讀整個 log 檔，回傳所有計時行。其他行（request log 等）略過；壞掉的行（寫到一半）也略過
// 逐行讀、先用 bytes.Contains 快篩，不對每一行都 unmarshal
func readTimingLines(path string) ([]timingRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []timingRecord
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, timingNeedle) {
			var rec timingRecord
			if json.Unmarshal(line, &rec) == nil && rec.Msg == loadtiming.LogMessage {
				out = append(out, rec)
			}
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// ws hub 踢慢 client 時 log 的訊息（internal/ws/hub.go enqueueLocked）；test 會檢查 hub.go 裡真的有這段字
const slowKickText = "kicking slow client"

// 讀 log 檔，回傳所有「踢慢 client」那幾行的時間（log 的 time 欄位）。時間讀不出來的行記成零值時間，
// 這樣整個檔案的總數不會少算，只是它不會落在任何 step 的窗口裡
func readSlowKickTimes(path string) ([]time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	needle := []byte(slowKickText)
	r := bufio.NewReaderSize(f, 1<<20)
	var out []time.Time
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, needle) {
			var rec struct {
				Time time.Time `json:"time"`
			}
			json.Unmarshal(line, &rec) // 壞掉的行 → 零值時間
			out = append(out, rec.Time)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// 落在 [from, to] 的時間數
func countInWindow(ts []time.Time, from, to time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(from) && !t.After(to) {
			n++
		}
	}
	return n
}

// 一個 log 檔對一個 step 窗口的彙整
type timingSummary struct {
	Path           string  `json:"log_file"`
	Lines          int     `json:"lines"`
	AcquireCount   int64   `json:"pool_acquire_count"`
	AcquireTotalMs float64 `json:"pool_acquire_total_ms"`
	EmptyAcquire   int64   `json:"pool_empty_acquire_count"`
	CanceledAcq    int64   `json:"pool_canceled_acquire_count"`
	AcquiredMax    int32   `json:"pool_acquired_max"` // 各行「當下」值的最大 / 最小（每 5 秒才取一次），不是區間內的真正峰值，短暫的尖峰可能沒被看到
	IdleMin        int32   `json:"pool_idle_min"`
	MaxConns       int32   `json:"pool_max_conns"`
	CreateCount    int64   `json:"create_message_count"`
	CreateErrors   int64   `json:"create_message_errors"`
	CreateMeanMs   float64 `json:"create_message_mean_ms"`
	CreateP50Ms    float64 `json:"create_message_p50_ms_upper_bound"` // bucket 上界；-1 = 超過最大邊界
	CreateP99Ms    float64 `json:"create_message_p99_ms_upper_bound"`

	// 每次 acquire 的平均耗時 = acquire_total / acquire_count（分母是「全部」acquire，因為 acquire_total 包含全部）；沒有 acquire 時是 0
	// 注意 acquire_count 可能含少量非 CreateMessage 的 acquire（例如 healthcheck），而且 pool 計數和 CreateMessage 直方圖
	// 的窗口 / bucket 對不齊，所以 query_mean = create_mean - acquire_mean 是粗略拆法（約略值），極端時仍可能略為負值，不會被截斷
	AcquireMeanMs float64 `json:"pool_acquire_mean_ms_per_acquire"`
	QueryMeanMs   float64 `json:"query_mean_ms_create_minus_acquire_approx"` // create_mean - acquire_mean（約略）

	LimiterCount  int64   `json:"limiter_allow_count"`
	LimiterErrors int64   `json:"limiter_allow_errors"`
	LimiterMeanMs float64 `json:"limiter_allow_mean_ms"` // 每次 Allow；一個 POST 會問兩條規則
	PublishCount  int64   `json:"redis_publish_count"`
	PublishErrors int64   `json:"redis_publish_errors"`
	PublishMeanMs float64 `json:"redis_publish_mean_ms"`

	// server log 裡 "ws outbox full, kicking slow client" 的行數：落在量測窗口內的；整個 log 檔（整個 run）的見 runReport
	SlowKicksInWindow int `json:"slow_client_kicks_in_window"`

	Raw   []loadtiming.Line `json:"intervals"`
	Notes []string          `json:"notes,omitempty"`
}

// 彙整落在窗口 [from, to] 的行。一行代表它結束時間往前 interval_ms 的區間；
// 區間與窗口重疊至少一半才採用（server 的 5 秒節拍和 step 窗口對不齊，這是近似）
func summarizeTiming(path string, recs []timingRecord, from, to time.Time) timingSummary {
	s := timingSummary{Path: path, IdleMin: -1}
	merged := make([]int64, len(loadtiming.BoundsMs)+1)
	for _, rec := range recs {
		d := time.Duration(rec.Data.IntervalMs) * time.Millisecond
		if d <= 0 {
			continue
		}
		end, begin := rec.Time, rec.Time.Add(-d)
		overlapEnd, overlapBegin := end, begin
		if to.Before(overlapEnd) {
			overlapEnd = to
		}
		if from.After(overlapBegin) {
			overlapBegin = from
		}
		overlap := overlapEnd.Sub(overlapBegin)
		if overlap*2 < d {
			continue
		}
		if !slices.Equal(rec.Data.Create.BoundsMs, loadtiming.BoundsMs) || len(rec.Data.Create.Buckets) != len(merged) {
			s.Notes = append(s.Notes, "a line had different histogram bounds and was skipped")
			continue
		}
		s.Lines++
		p, c := rec.Data.Pool, rec.Data.Create
		s.AcquireCount += p.AcquireCount
		s.AcquireTotalMs += p.AcquireDurationMs
		s.EmptyAcquire += p.EmptyAcquireCount
		s.CanceledAcq += p.CanceledAcquireCount
		s.AcquiredMax = max(s.AcquiredMax, p.Acquired)
		if s.IdleMin < 0 || p.Idle < s.IdleMin {
			s.IdleMin = p.Idle
		}
		s.MaxConns = p.Max
		s.CreateCount += c.Count
		s.CreateErrors += c.Errors
		s.CreateMeanMs += c.SumMs
		s.LimiterCount += rec.Data.Limiter.Count
		s.LimiterErrors += rec.Data.Limiter.Errors
		s.LimiterMeanMs += rec.Data.Limiter.SumMs
		s.PublishCount += rec.Data.Publish.Count
		s.PublishErrors += rec.Data.Publish.Errors
		s.PublishMeanMs += rec.Data.Publish.SumMs
		for i, n := range c.Buckets {
			merged[i] += n
		}
		s.Raw = append(s.Raw, rec.Data)
	}
	if s.CreateCount > 0 {
		s.CreateMeanMs /= float64(s.CreateCount)
	}
	if s.LimiterCount > 0 {
		s.LimiterMeanMs /= float64(s.LimiterCount)
	}
	if s.PublishCount > 0 {
		s.PublishMeanMs /= float64(s.PublishCount)
	}
	if s.AcquireCount > 0 {
		s.AcquireMeanMs = s.AcquireTotalMs / float64(s.AcquireCount)
	}
	s.QueryMeanMs = s.CreateMeanMs - s.AcquireMeanMs
	s.CreateP50Ms = loadtiming.Quantile(merged, 0.50)
	s.CreateP99Ms = loadtiming.Quantile(merged, 0.99)
	if s.Lines == 0 {
		s.Notes = append(s.Notes, "no timing lines overlapped the window (is LOADTEST_TIMING_LOG=1 set on that server?)")
	}
	return s
}
