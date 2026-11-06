package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 真正的送出：POST /rooms/{id}/messages。每一筆依 seq 輪流選 server、房間與使用者，
// 讓每個 user 的送出頻率遠低於限流，限流不會變成瓶頸

type poster struct {
	client  *http.Client
	servers []string // http base URL
	rooms   []string // 這個 step 用到的房間（public id）
	tokens  []string // 每個使用者一個 access token；絕不印出
	run     string
	step    int
}

// seq 對應的房間索引。sampled 連線算 expected 時要用同一個公式
func roomIndex(seq, rooms int) int {
	return seq % rooms
}

// 第 i 個項目（連線序號或 POST 的 seq）連到哪一台 server。房間是 i % rooms，所以 server 不能也用 i % servers：
// 房間數和 server 數有公因數（例如 2 個房間 2 台 server）時，房間和 server 會綁死，同一個房間永遠只在同一台，
// 跨 instance 的路徑（POST 到 A、廣播經 Redis 到 B 的 viewer）就沒被測到。改用 (i / rooms) % servers，
// 每個房間輪流落在每一台 server 上；只有 1 個房間時跟 i % servers 一樣
func serverIndex(i, rooms, servers int) int {
	return (i / max(rooms, 1)) % servers
}

// 給 open-loop 的 sendFunc。回應 body 讀完丟掉，讓連線可以重用
func (p *poster) send(ctx context.Context, seq int) (int, error) {
	base := p.servers[serverIndex(seq, len(p.rooms), len(p.servers))]
	room := p.rooms[roomIndex(seq, len(p.rooms))]
	token := p.tokens[seq%len(p.tokens)]
	body := fmt.Sprintf(`{"content":%q}`, makeMarker(p.run, p.step, seq))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rooms/"+room+"/messages", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// 送 POST 用的 HTTP client：不走環境 proxy，keep-alive 的閒置連線數跟 in-flight 上限一致
func newHTTPClient(maxInFlight int, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.MaxIdleConns = maxInFlight
	tr.MaxIdleConnsPerHost = maxInFlight
	tr.DisableCompression = true
	return &http.Client{Transport: tr, Timeout: timeout}
}

// 開跑前確認每台 server 都活著，而且連的是同一個 DB（種的房間查得到）：
// 否則 POST 會全部 404，整個 run 白跑
func preflight(ctx context.Context, client *http.Client, servers []string, room string) error {
	for _, base := range servers {
		for _, path := range []string{"/healthcheck", "/rooms/" + room} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("GET %s%s: %w", base, path, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				if strings.HasPrefix(path, "/rooms/") {
					return fmt.Errorf("%s does not see the seeded room (status %d): is it using the same TEST_DATABASE_URL database?", base, resp.StatusCode)
				}
				return fmt.Errorf("%s%s returned %d", base, path, resp.StatusCode)
			}
		}
	}
	return nil
}
