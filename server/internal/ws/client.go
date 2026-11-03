// Package ws 是 WebSocket 的傳輸層：只管 conn、outbox、keepalive，只處理 []byte，
// 不認識 JSON protocol（envelope），也不知道任何 registry / Hub
package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMessageBytes = 500
	writeWait       = 10 * time.Second
	// 每條連線 outbox（Client.send）的容量：滿了 = 這條連線消化不了 → 踢掉，讓 client 重連後補撈歷史
	// 不開大：越大代表 client 落後越多才被踢（熱門房每秒 50 則時，256 ≈ 落後 5 秒）
	SendBuffer = 256
)

// 一條連線的傳輸層：只擁有 conn、send（outbox）、done
// 發現連線壞了只 Close() 叫醒讀取迴圈，絕不自己從任何 map 移除或呼叫 Redis（清理 owner 是呼叫 ReadLoop 的那一方）
// 寫 conn 不用拿鎖：gorilla 同一條連線只能一個 writer，這個 writer 就是 WriteLoop，其他人一律用 Enqueue 排進 send
type Client struct {
	conn *websocket.Conn
	// outbox：要寫給這條連線的 frame（已經編碼好的 bytes），只有 WriteLoop 會讀出來寫進 conn
	// 不 close：往裡面送的有好幾個人（廣播、讀取迴圈），close 了別人再送會 panic；WriteLoop 靠 done 結束
	send chan []byte
	// 由 Shutdown close 一次，同時叫醒 WriteLoop 和 PingLoop
	// 類似在kotlin裡面呼叫 `job.cancel()`的感覺
	done       chan struct{}
	pingPeriod time.Duration
	pongWait   time.Duration
	logger     *slog.Logger
}

// 不碰 conn（只是存起來）：keepalive 的設定在 ReadLoop 開頭做
// logger 在這裡才取 slog.Default()：main 會 slog.SetDefault，package 初始化時取會拿到舊的
func NewClient(conn *websocket.Conn, pingPeriod, pongWait time.Duration) *Client {
	return &Client{
		conn:       conn,
		send:       make(chan []byte, SendBuffer),
		done:       make(chan struct{}),
		pingPeriod: pingPeriod,
		pongWait:   pongWait,
		logger:     slog.Default().With("component", "websocket"),
	}
}

// 讀取迴圈：先設定 keepalive，再一直 ReadMessage 直到失敗，失敗就 return（清理由呼叫端在 return 後做）
// 每收到一個 frame 都延長 read deadline（收到任何訊息都代表 client 還活著），再交給 onMessage
// 限流不在這裡：由呼叫端包在 onMessage 裡（被丟掉的 frame 一樣算「活著」）
func (c *Client) ReadLoop(ctx context.Context, onMessage func(msg []byte)) {
	// 連線成功，設定每次發送的上限
	c.conn.SetReadLimit(maxMessageBytes)
	// 加上pingpong的keepalive mechanism
	if err := c.addPongReadDeadline(); err != nil {
		// 不 return：addPongReadDeadline 失敗時已經 Close，讓下面的 ReadMessage 失敗後 return，呼叫端才會去 removeClient
		// 在這裡 return 一樣會讓呼叫端走清理，但維持跟原本一樣的流程
		c.logger.WarnContext(ctx, "set read deadline failed", "error", err)
	}

	// 掛上pong handler
	c.conn.SetPongHandler(func(appData string) error {
		err := c.addPongReadDeadline()
		return err
	})

	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		// 收到任何訊息都代表 client 還活著
		if err := c.addPongReadDeadline(); err != nil {
			c.logger.WarnContext(ctx, "set read deadline failed", "error", err)
		}
		// auth frame 裡有 access token，不能進 log；誰送的看身分變化的 INFO log
		c.logger.DebugContext(ctx, "frame received", "bytes", len(msg))
		onMessage(msg)
	}
}

func (c *Client) addPongReadDeadline() error {
	t := time.Now().Add(c.pongWait)
	err := c.conn.SetReadDeadline(t)
	if err != nil {
		c.conn.Close()
		return err
	}
	return nil
}

func (c *Client) PingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			// 直接結束goroutine，
			// 因為done是在Shutdown中被close的
			// 也就是說會進到這狀況，大家都知道connection已經死了
			// 所以不需要再次handle
			return
		case <-ticker.C:
			// 送ping
			deadline := time.Now().Add(writeWait)
			if err := c.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				// 送ping的時候遇到錯誤，代表該connection遇到問題
				// 也可能是等 gorilla 內部寫入鎖逾時（WriteLoop 正卡在寫這條慢 client，最多 writeWait）：一律視為連線不健康、關掉
				c.logger.WarnContext(ctx, "write ping failed", "error", err)
				// 特別注意，因為PingLoop跟ReadLoop在不同go routine
				// 所以這邊要特別指定close，不然ReadLoop要等到timeout
				// 才會繼續走
				c.Close()
				return
			}
		}

	}
}

// 每條連線一個：唯一會把資料 frame 寫進 conn 的 goroutine
// 結束條件：done 被 close（讀取迴圈結束、Shutdown 被呼叫），或寫入失敗
// 失敗時只 Close 叫醒讀取迴圈，不自己 removeClient（清理 owner 是讀取迴圈）
//
// 已知、無害的多餘 warn log（"write frame failed"）：
//   - 被踢時：WriteLoop 正卡在寫入，conn 一被 Close 這次寫入就失敗 → 跟 "outbox full" 各一行
//   - 正常斷線時：done 被 close 的同時 send 裡還有東西，select 是隨機選 → 可能選到 send、寫已關閉的 conn
func (c *Client) WriteLoop(ctx context.Context) {
	for {
		select {
		case <-c.done:
			// 收到done，已經完成，直接結束
			return
		case msg := <-c.send:
			// 收到send過來的訊息，寫入msg
			err := c.writeRaw(msg)
			if err != nil {
				c.logger.WarnContext(ctx, "write frame failed", "error", err)
				c.Close()
				return
			}
		}
	}
}

// 真的把一則訊息寫進 conn（只有 WriteLoop 呼叫，不拿鎖）
// SetWriteDeadline 或 WriteMessage 任一失敗就回傳 error，由 WriteLoop 負責 Close
func (c *Client) writeRaw(msg []byte) error {
	maxWaitingTime := time.Now().Add(writeWait)
	if err := c.conn.SetWriteDeadline(maxWaitingTime); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// 非阻塞排進 outbox，絕不等
// true = 已排進去；false = outbox 滿了（慢 client），由呼叫端記 log 並呼叫 Close 踢掉
// 這裡不 log、不 close
func (c *Client) Enqueue(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

// 只關底層 conn（重複呼叫無害）：用來踢人或叫醒讀取迴圈
// 不能 close done：這時讀取迴圈還在跑，之後的 Shutdown 會 double close 而 panic
func (c *Client) Close() {
	c.conn.Close()
}

// 讀取迴圈 return 之後由 HandleWebsocket 用 defer 呼叫，只能呼叫一次：close(done)（讓 PingLoop、WriteLoop 結束）再 conn.Close()
func (c *Client) Shutdown() {
	close(c.done)
	c.conn.Close()
}
