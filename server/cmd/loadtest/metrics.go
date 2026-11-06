package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 從 server 自己的 /metrics（Prometheus 文字格式，公開、唯讀的本機 GET）讀 POST /rooms/{public_id}/messages 的
// http_request_duration_seconds sum / count，量測窗口起點與終點各讀一次，相減得到 handler 的平均耗時。
// 這是 middleware 看到的整個 handler（含 auth、限流、DB、PUBLISH），拿來和 DB / limiter / publish 的分項相減，就看得到沒被分項解釋的時間。
// 限制：histogram 不分 status（201 / 429 / 4xx 都混在一起）；起點與終點各有幾 ms 的讀取時間、窗口邊界上還在處理的請求會被算進或漏掉，所以是近似值。

const (
	postDurationMetric = "http_request_duration_seconds"
	postRouteLabel     = `path="/rooms/{public_id}/messages"`
	postMethodLabel    = `method="POST"`
)

type durationSnapshot struct {
	SumSeconds float64
	Count      int64
}

// 從 /metrics 的文字內容挑出 POST messages 那一組的 _sum 與 _count；找不到 = ok false（還沒有人打過這條 route 時就是這樣）
func parsePostDuration(r io.Reader) (snap durationSnapshot, ok bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var isSum bool
		switch {
		case strings.HasPrefix(line, postDurationMetric+"_sum{"):
			isSum = true
		case strings.HasPrefix(line, postDurationMetric+"_count{"):
		default:
			continue
		}
		if !strings.Contains(line, postMethodLabel) || !strings.Contains(line, postRouteLabel) {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			return snap, false, fmt.Errorf("unexpected metrics line %q", line)
		}
		v, perr := strconv.ParseFloat(line[i+1:], 64)
		if perr != nil {
			return snap, false, fmt.Errorf("unexpected metrics line %q", line)
		}
		if isSum {
			snap.SumSeconds = v
		} else {
			snap.Count = int64(v)
		}
		ok = true
	}
	return snap, ok, sc.Err()
}

// GET base/metrics 一次
func fetchPostDuration(ctx context.Context, client *http.Client, base string) (durationSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return durationSnapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return durationSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return durationSnapshot{}, fmt.Errorf("GET /metrics returned %d", resp.StatusCode)
	}
	snap, _, err := parsePostDuration(resp.Body) // 找不到 = 還沒有 POST，視為 0
	return snap, err
}

type handlerTiming struct {
	Server string  `json:"server"`
	Count  int64   `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	Note   string  `json:"note,omitempty"`
}

// 窗口內 POST handler 的耗時：每台 server 一筆，加上合併（以筆數加權）的平均
type handlerSummary struct {
	Servers []handlerTiming `json:"servers,omitempty"`
	Count   int64           `json:"count"`
	MeanMs  float64         `json:"mean_ms"`
}

// 起點 / 終點的快照相減。任何一邊缺（讀取失敗）就在該 server 的 Note 寫原因，不算進合併平均
func summarizeHandler(servers []string, before, after map[string]durationSnapshot, errs map[string]error) handlerSummary {
	var out handlerSummary
	var sumSec float64
	for _, base := range servers {
		h := handlerTiming{Server: base}
		b, okB := before[base]
		a, okA := after[base]
		switch {
		case errs[base] != nil:
			h.Note = "cannot read /metrics: " + errs[base].Error()
		case !okB || !okA:
			h.Note = "missing a /metrics snapshot"
		case a.Count < b.Count || a.SumSeconds < b.SumSeconds:
			h.Note = "counters went backwards (server restarted?)"
		default:
			h.Count = a.Count - b.Count
			if h.Count > 0 {
				h.MeanMs = (a.SumSeconds - b.SumSeconds) / float64(h.Count) * 1000
			}
			out.Count += h.Count
			sumSec += a.SumSeconds - b.SumSeconds
		}
		out.Servers = append(out.Servers, h)
	}
	if out.Count > 0 {
		out.MeanMs = sumSec / float64(out.Count) * 1000
	}
	return out
}

// 對每台 server 各讀一次
func snapshotAllHandlers(ctx context.Context, client *http.Client, servers []string) (map[string]durationSnapshot, map[string]error) {
	snaps := map[string]durationSnapshot{}
	errs := map[string]error{}
	for _, base := range servers {
		s, err := fetchPostDuration(ctx, client, base)
		if err != nil {
			errs[base] = err
			continue
		}
		snaps[base] = s
	}
	return snaps, errs
}
