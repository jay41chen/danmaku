package main

import "time"

// sampled 連線的收件統計：send-to-receive 延遲（從排程時間量起）與漏收數

type sampleInput struct {
	Interval      time.Duration
	FirstMeasured int
	Total         int
	// 這個 sample 連線應該收到第 seq 筆嗎（量測期間、server 回 201、而且是這條連線所在的房間）
	Expected func(sample, seq int) bool
	// 這個 sample 連線收到第 seq 筆的時間位移；沒收到 ok=false
	Received func(sample, seq int) (time.Duration, bool)
	// 哪些 sample 連線真的有進房間（沒進房的不算 expected）
	Joined []bool
}

type receiveSummary struct {
	Latency  latencySummary `json:"send_to_receive"`
	Expected int            `json:"expected"`
	Received int            `json:"received"`
	Missing  int            `json:"missing"`
	Samples  int            `json:"sampled_connections"`
}

// 整理 sampled 連線的收件統計。延遲 = 收到時間 - 排程時間（schedOffset = seq * Interval）
func summarizeReceive(in sampleInput) receiveSummary {
	var out receiveSummary
	var lats []time.Duration
	for sample, joined := range in.Joined {
		if !joined {
			continue
		}
		out.Samples++
		for seq := in.FirstMeasured; seq < in.Total; seq++ {
			if !in.Expected(sample, seq) {
				continue
			}
			out.Expected++
			off, ok := in.Received(sample, seq)
			if !ok {
				out.Missing++
				continue
			}
			out.Received++
			lats = append(lats, max(off-time.Duration(seq)*in.Interval, 0))
		}
	}
	out.Latency = summarize(lats)
	return out
}
