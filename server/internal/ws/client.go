// Package ws 是 WebSocket 的傳輸層（Client）和連線註冊表（Hub）：只處理 []byte，不認識 JSON protocol（envelope）
// Client 只管 conn、outbox、keepalive，不知道 Hub 的存在；Hub 管連線在哪間房、身分與到期
package ws

import (
	"context"
	"log/slog"
	"sync"
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
	// 由 Close close（closeOnce 保證只一次），同時叫醒 WriteLoop 和 PingLoop
	// 類似在kotlin裡面呼叫 `job.cancel()`的感覺
	done       chan struct{}
	pingPeriod time.Duration
	pongWait   time.Duration
	logger     *slog.Logger
	// 讓 Close 重複呼叫也只 close(done) 一次（第二次 close 會 panic）；每個 Client 一份，跟著它保護的 done 走
	closeOnce sync.Once
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
		// 不 return：addPongReadDeadline 失敗時已經 Close，下面的 ReadMessage 會失敗後 return，呼叫端接著 Unregister
		// 在這裡 return 一樣會讓呼叫端走清理，只是讓 ReadLoop 維持只有一個 return 出口
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
		c.Close()
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
			// done 被 close = 有人呼叫了 Close（連線已經或正在關掉），直接結束，不需要再處理
			return
		case <-ticker.C:
			// 送ping
			deadline := time.Now().Add(writeWait)
			if err := c.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				// 送失敗先看 done（跟 WriteLoop 同一個模式）
				select {
				case <-c.done:
					// 別人先 Close 了（done 和 ticker 同時就緒時 select 隨機選，可能選到 ticker）
					// 失敗是預期中的，不log
					return
				default:
					// 真的出錯：連線有問題，或等 gorilla 內部寫入鎖逾時（WriteLoop 正卡在寫這條慢 client，最多 writeWait）
					// 一律視為不健康、關掉
					// PingLoop 跟 ReadLoop 在不同 goroutine，要主動 Close，不然 ReadLoop 要等到 pongWait 逾時才醒
					c.logger.WarnContext(ctx, "write ping failed", "error", err)
					c.Close()
					return
				}
			}
		}

	}
}

// 每條連線一個：唯一會把資料 frame 寫進 conn 的 goroutine
// 結束條件：done 被 close（有人呼叫 Close），或寫入失敗
// 失敗時只 Close 叫醒讀取迴圈，不自己 Unregister（清理 owner 是讀取迴圈）
// 寫入失敗時先看 done：已經 close = 別人先關的（被踢、正常斷線），失敗是預期中的 → 不 log
// 這要靠 Close 先 close(done) 再關 conn：conn 一關寫入就失敗，那時 done 必須已經 close
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
				// 沒辦法預測writeRaw會花多少時間，所以還是要判斷client的狀況
				select {
				case <-c.done:
					// 能馬上讀到 = done 已經 close
					return
				default:
					// 讀不到 = done 還沒 close
					c.logger.WarnContext(ctx, "write frame failed", "error", err)
					c.Close()
					return
				}
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
	// Enqueue：「send 能不能馬上塞進去？能就塞，不能就走 default」
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

// 關掉這條連線：重複呼叫、多個 goroutine 同時呼叫都安全（closeOnce）
// 踢人、叫醒讀取迴圈、HandleWebsocket 的 defer 都用這個
// 順序：先 close(done)（叫醒 PingLoop、閒置的 WriteLoop），再關 conn（叫醒卡在讀寫的 ReadLoop / WriteLoop）
// 不能反過來：WriteLoop 寫入失敗時靠 done 判斷是不是別人關的，conn 先關的話它會看到 done 還開著 → 多印一行 warn
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		// 關閉c.done，會停止[WriteLoop]跟[PingLoop]
		close(c.done)
		// 關閉websocket連線
		c.conn.Close()
	})
}
