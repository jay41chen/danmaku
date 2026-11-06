package main

import (
	"net/http"
	"time"
)

// 一個 step 量測期間（暖機之後）的送出統計
type sendSummary struct {
	Measured     int            `json:"measured"`  // 量測期間排程的筆數
	Responses    int            `json:"responses"` // 收到 HTTP 回應（任何 status）
	Errors       int            `json:"errors"`    // 傳輸錯誤 / timeout
	Dropped      int            `json:"dropped"`   // 超過 in-flight 上限，沒送
	NotRun       int            `json:"not_run"`   // 被取消沒執行
	Created      int            `json:"created"`   // 201
	RateLimited  int            `json:"rate_limited_429"`
	StatusCounts map[string]int `json:"status_counts"`
	// 所有「有結果」的請求，從排程時間量起：收到回應的（任何 status），加上 timeout / 傳輸錯誤的。
	// 錯誤的那筆延遲只是「等了多久才放棄」，是真實延遲的下限（真正要多久不知道）；不算進去的話，卡死的 server 會有好看的 p99
	PostAll     latencySummary `json:"post_latency_all"`
	PostCreated latencySummary `json:"post_latency_201"` // 只算 201
	Lag         latencySummary `json:"scheduling_lag"`   // 工具自己的排程延遲
}

// 從 open-loop 結果整理量測期間的統計；暖機（seq < FirstMeasured）不計入
func summarizeSends(res *openLoopResult) sendSummary {
	s := sendSummary{StatusCounts: map[string]int{}}
	var all, created, lag []time.Duration
	for seq := res.FirstMeasured; seq < res.Total; seq++ {
		s.Measured++
		switch res.Outcome[seq] {
		case outcomeNotRun:
			s.NotRun++
			continue
		case outcomeDropped:
			s.Dropped++
		case outcomeError:
			s.Errors++
			s.StatusCounts["error"]++
			all = append(all, time.Duration(res.LatencyNs[seq])) // 下限，見 PostAll
		case outcomeResponse:
			s.Responses++
			status := int(res.Status[seq])
			s.StatusCounts[http.StatusText(status)+" "+itoa(status)]++
			all = append(all, time.Duration(res.LatencyNs[seq]))
			switch status {
			case http.StatusCreated:
				s.Created++
				created = append(created, time.Duration(res.LatencyNs[seq]))
			case http.StatusTooManyRequests:
				s.RateLimited++
			}
		}
		lag = append(lag, time.Duration(res.LagNs[seq]))
	}
	s.PostAll = summarize(all)
	s.PostCreated = summarize(created)
	s.Lag = summarize(lag)
	return s
}
