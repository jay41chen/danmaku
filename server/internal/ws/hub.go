package ws

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	prom "github.com/jay41chen/danmaku/server/internal/prometheus"
)

const (
	// 身分變更 log 用的 from / to（有登入時用 user id）
	anonymous = "anonymous"

	// 身分變更的原因（log 的 reason 欄位）
	reasonAuth    = "auth"
	reasonExpired = "expired"
	// 唯一一個由 handler 傳進來的原因（auth frame 驗證失敗），所以匯出
	ReasonAuthFailed = "auth_failed"
)

// 連線的 registry：這台 server 上有哪些連線、誰是會員（refcount）、誰在哪個房間、token 什麼時候到期
//
// 整個 Hub 只有一把鎖（mu），保護所有 registry / 身分 / 房間狀態；Client 自己不留鎖
// 為什麼是一把全域鎖：每個操作都只是改 map / 欄位、排 outbox，拿著它的時間很短，所以暫時不另外加每連線的鎖
// 每連線的鎖留到 W6（見 ROADMAP），到時的鎖順序是 全域 → 每連線
//
// 鎖規則：絕不在持有 mu 時做 I/O 或呼叫外面的程式碼 —— Redis、DB、寫 conn 都不行
// 能做的只有改 map / 欄位、Client.Enqueue（非阻塞）、Client.Close（踢人：close(done) + 關 conn，不阻塞、可重複呼叫）
// 這也是為什麼 Hub 的方法收的是已經編碼好的 []byte，而不是 callback / func：
// 在鎖裡面呼叫外面的 method，一來 sync.Mutex 不可重入（外面的程式碼又回頭呼叫 Hub 就死鎖），
// 二來可能在鎖裡做 I/O，三來外面的程式碼可以在鎖裡破壞 Hub 的不變量
// 所以 I/O 一律在鎖外做完，再呼叫 Hub 更新狀態、排 frame（排 frame 只是排進 outbox，不寫 conn）
// Hub 也不認識 JSON protocol，同理 handler 要自己把 envelope 編成 []byte 再交給 Hub
//
// 房間（clients[c].room 與 rooms 索引）的不變量：
// 只有讀取迴圈這個 goroutine 會改變 client 所在的房間，而 Unregister 一定在讀取迴圈結束後才呼叫
// → Hub 記的房間就是 Redis 那邊欠的 LeaveRoom。
// 哪天讀取迴圈以外的地方要改房間（例如管理員踢出房），這條就破了，要重新評估。
//
// 命名慣例：結尾是 Locked 的 function 都要求呼叫端已經持有 h.mu
type Hub struct {
	mu         sync.Mutex
	maxClients int
	clients    map[*Client]*clientState        // 已註冊的連線
	rooms      map[string]map[*Client]struct{} // 房間 public_id → 在裡面的連線（廣播用的索引，跟 clientState.room 同步）
	members    map[int64]int                   // user id → 這個 user 在這台 server 上的連線數（同一個人多分頁只算一個 viewer）
	logger     *slog.Logger
	// presence 快照可能變了的通知（buffer 1，非阻塞送）：worker 醒來後自己呼叫 PresenceSnapshot 取最新的
	presenceWake chan struct{}
}

// 一條連線的狀態，全部由 Hub.mu 保護。userID == 0 代表匿名（users.id 是 IDENTITY，從 1 開始，不會有 0）
type clientState struct {
	// 目前所在房間的 public_id，空字串 = 不在任何房間
	room     string
	userID   int64
	username string
	exp      time.Time
	timer    *time.Timer
}

// logger 在這裡才取 slog.Default()：main 會在建立 handler 之前 slog.SetDefault，package 初始化時取會拿到舊的
func NewHub(maxClients int) *Hub {
	return &Hub{
		maxClients:   maxClients,
		clients:      make(map[*Client]*clientState),
		rooms:        make(map[string]map[*Client]struct{}),
		members:      make(map[int64]int),
		presenceWake: make(chan struct{}, 1),
		logger:       slog.Default().With("component", "websocket"),
	}
}

// 新連線一律是匿名：這裡只加進 clients、總連線數 +1，refcount 不動（Authenticate 成功才 memberJoinedLocked）
// 回傳 false = 連線數已滿，沒加進去（呼叫端要直接 return）
//
// TODO(W6): clients / members 都是這個 process 自己記憶體裡的 map，只算得到「這台 server」的連線
// 同一個 user 一條連 server-1、一條連 server-2 → 兩台各回報 member=1，Grafana 加總變 2（其實 1 人）
// 跨 server 去重要放到共用的地方（Redis）+ TTL + heartbeat：server 被 kill -9 時沒機會清，
// 只能靠「活著的定期續命、死掉的自然過期」，見 ROADMAP W6「跨 server 的不同會員數」
func (h *Hub) Register(ctx context.Context, c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.maxClients {
		h.logger.WarnContext(ctx, "too many connections")
		return false
	}
	h.clients[c] = &clientState{}
	prom.WebsocketConnections.Inc()
	prom.WebsocketConnectionsTotal.Inc()
	h.updateViewersGaugeLocked()
	// request_id 在 upgrade 時產生，同一條連線的所有 log 共用，等同 connection ID
	h.logger.InfoContext(ctx, "new client connected", "total", len(h.clients))
	return true
}

// 斷線：停 timer、是會員就 refcount -1、離開房間索引、總連線數 -1
// 回傳這條連線斷線時所在的房間（"" = 不在任何房間）：呼叫端要在鎖外對 Redis LeaveRoom
// 不變量見 Hub 的說明：只有讀取迴圈會改房間、Unregister 在讀取迴圈結束後才呼叫，所以這裡記的房間就是 Redis 欠的 LeaveRoom
// 現在只有讀取迴圈結束後的清理會呼叫；重複呼叫不會重複扣、回傳 ""（防禦用）
//
// TODO(W6): 同 Register，只扣得到這台 server 的數字；process 直接死掉時這裡根本不會執行，
// 換成 Redis 版本後要靠 TTL 清掉死掉那台留下的資料
func (h *Hub) Unregister(ctx context.Context, c *Client) (room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room = h.removeClientLocked(c)
	h.logger.InfoContext(ctx, "client disconnected", "total", len(h.clients))
	return room
}

func (h *Hub) removeClientLocked(c *Client) string {
	st, ok := h.clients[c]
	if !ok {
		// already removed
		return ""
	}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if st.userID != 0 {
		h.memberLeftLocked(st.userID)
		st.userID = 0
	}
	room := st.room
	h.removeFromRoomLocked(c, room)
	st.room = ""
	h.notifyPresenceLocked()
	prom.WebsocketConnections.Dec()
	delete(h.clients, c)
	h.updateViewersGaugeLocked()
	return room
}

// 這條連線目前所在的房間；不在房間或沒註冊 = ""
func (h *Hub) Room(c *Client) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return ""
	}
	return st.room
}

// JoinRoom（Redis）成功後呼叫：記下房間、加進廣播索引、排 joinOK
// 回傳 false = c 沒註冊（防禦用），什麼都沒改；joinOK == nil 代表只改狀態、不送 frame
func (h *Hub) Join(ctx context.Context, c *Client, room string, joinOK []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return false
	}
	h.removeFromRoomLocked(c, st.room)
	st.room = room
	members := h.rooms[room]
	if members == nil {
		members = make(map[*Client]struct{})
		h.rooms[room] = members
	}
	members[c] = struct{}{}
	h.notifyPresenceLocked()
	h.sendLocked(ctx, c, st, joinOK)
	return true
}

// 離開房間：清掉房間、移出廣播索引、排 leaveOK（nil = 不送）。沒註冊 = no-op
// Redis 的 LeaveRoom 由呼叫端在鎖外做
func (h *Hub) Leave(ctx context.Context, c *Client, leaveOK []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return
	}
	h.removeFromRoomLocked(c, st.room)
	st.room = ""
	h.notifyPresenceLocked()
	h.sendLocked(ctx, c, st, leaveOK)
}

// token 驗證通過後套用身分並排 authOK：
//   - 匿名 → 該 user；member → 同一個 user = 續期；member → 不同 user = 直接換人
//
// 同時排程 exp 到期的 timer，到期時降回匿名並排 expired（nil = 只降級不送）
// 沒註冊 = no-op（防禦用）
func (h *Hub) Authenticate(ctx context.Context, c *Client, userID int64, username string, exp time.Time, authOK, expired []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return
	}
	if st.userID != userID {
		from := st.userID
		if from != 0 {
			h.memberLeftLocked(from)
		}
		st.userID = userID
		h.memberJoinedLocked(userID)
		h.notifyPresenceLocked()
		h.logger.InfoContext(ctx, "ws identity changed", "from", identityLabel(from), "to", identityLabel(userID), "reason", reasonAuth)
	}
	// 同一個 user 續期不算身分變更，只更新 exp 和 timer
	st.username = username
	st.exp = exp
	h.updateViewersGaugeLocked()
	h.scheduleExpiryLocked(ctx, c, st, expired)

	h.sendLocked(ctx, c, st, authOK)
}

// 把這條連線降回匿名（已經是匿名就不改身分），並排 reply（nil = 不送）
// reply 只要 c 有註冊就會排，不管這次有沒有真的降級 —— 呼叫端（auth 失敗）一律要回 error frame
// 沒註冊 = no-op
func (h *Hub) Downgrade(ctx context.Context, c *Client, reason string, reply []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return
	}
	h.downgradeLocked(ctx, c, st, reason)
	h.sendLocked(ctx, c, st, reply)
}

// 目前在房間裡的連線快照：每條「已註冊且 room != ""」的連線一筆（連線 ID + 目前身分，匿名 = 0）
// 不在房間的連線不算。回傳的是新建的 map，呼叫端可以隨便用
func (h *Hub) PresenceSnapshot() map[model.PresenceEntry]struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := make(map[model.PresenceEntry]struct{})
	for c, st := range h.clients {
		if st.room == "" {
			continue
		}
		snap[model.PresenceEntry{Room: st.room, ConnID: c.ID, UserID: st.userID}] = struct{}{}
	}
	return snap
}

// 快照可能變了就會有訊號（合併過：多次變動只留一個）；worker 收到後再呼叫 PresenceSnapshot
func (h *Hub) PresenceChanged() <-chan struct{} {
	return h.presenceWake
}

// 通知 presence worker 快照可能變了（呼叫時要持有 h.mu）
// 非阻塞送：已經有未處理的訊號就直接丟掉，不能在鎖裡等
func (h *Hub) notifyPresenceLocked() {
	select {
	case h.presenceWake <- struct{}{}:
	default:
	}
}

// 排一則 frame 給這條連線。沒註冊（應該被移除了）就靜靜丟掉
func (h *Hub) Send(ctx context.Context, c *Client, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.clients[c]
	if !ok {
		return
	}
	h.sendLocked(ctx, c, st, frame)
}

// 把 frame 送給所有在 room 的連線
// 呼叫者有兩個：Redis consumer goroutine（整台只有一個，SUB 確認也靠它）和 presence worker
// 所以只能做不會卡的事，絕不寫 conn
// frame 這個 slice 是所有連線共用的，只讀不改（每條連線的 outbox 裡放的是同一個 slice）
func (h *Hub) Broadcast(ctx context.Context, room string, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[room] {
		h.enqueueLocked(ctx, c, room, frame)
	}
}

// 把 frame 送給 room 裡「身分不是 exceptUserID」的連線；匿名連線（userID 0）一定收得到
// 用途：user_hidden 不能發給被 ban 的本人（包含他開的每個分頁）。身分以 server 這邊 Hub 記的為準（auth frame 驗過的 token）
// 限制：本人若用匿名連線（沒送 auth frame）或另一個帳號看，server 認不出來，會收到
// 鎖規則同 Broadcast：只排 outbox、不做 I/O；frame 是共用的 slice，只讀不改
func (h *Hub) BroadcastExceptUser(ctx context.Context, room string, exceptUserID int64, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[room] {
		if st, ok := h.clients[c]; ok && exceptUserID != 0 && st.userID == exceptUserID {
			continue
		}
		h.enqueueLocked(ctx, c, room, frame)
	}
}

// 排 frame（呼叫時要持有 h.mu）；frame == nil = 呼叫端編碼失敗，什麼都不送
func (h *Hub) sendLocked(ctx context.Context, c *Client, st *clientState, frame []byte) {
	if frame == nil {
		return
	}
	h.enqueueLocked(ctx, c, st.room, frame)
}

// 把一則已編碼的 frame 交給這條連線的 WriteLoop（呼叫時要持有 h.mu）
// 絕不等：滿了 = 慢 client → Close 踢掉（關連線、叫醒讀取迴圈，清理交給它）
// 廣播和讀取迴圈（join_ok、pong、error）都走這裡，規則只有一條
// 被踢之後、讀取迴圈 Unregister 之前，新的訊息還是會排不進去 → 再記一行 log、再 Close 一次（重複 Close 無害）
func (h *Hub) enqueueLocked(ctx context.Context, c *Client, room string, msg []byte) {
	if !c.Enqueue(msg) {
		// 滿了，要斷線（讓 client 重連後補撈歷史）
		// 不帶 msg：那是使用者的訊息內容
		h.logger.WarnContext(ctx, "ws outbox full, kicking slow client", "room", room, "buffer", SendBuffer)
		c.Close()
	}
}

// 從房間索引移除（呼叫時要持有 h.mu）；room == "" 或不在裡面都是 no-op
func (h *Hub) removeFromRoomLocked(c *Client, room string) {
	if room == "" {
		return
	}
	members := h.rooms[room]
	delete(members, c)
	if len(members) == 0 {
		delete(h.rooms, room)
	}
}

// 把這條連線降回匿名（呼叫時要持有 h.mu）：停 timer、扣 refcount、記 log、更新 gauge
// 已經是匿名就什麼都不做。只改身分，不回 frame —— 回什麼 error 由呼叫端決定
func (h *Hub) downgradeLocked(ctx context.Context, c *Client, st *clientState, reason string) {
	if st.userID == 0 {
		return
	}
	from := st.userID
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	h.memberLeftLocked(from)
	st.userID = 0
	st.username = ""
	st.exp = time.Time{}
	h.notifyPresenceLocked()
	h.updateViewersGaugeLocked()
	h.logger.InfoContext(ctx, "ws identity changed", "from", identityLabel(from), "to", anonymous, "reason", reason)
}

// 排程 / 重設「token 到期就降回匿名」的 timer（呼叫時要持有 h.mu）
// 連線變成 member 或續期時都會呼叫
func (h *Hub) scheduleExpiryLocked(ctx context.Context, c *Client, st *clientState, expired []byte) {
	// 如果本來就有timer，那就先移除
	if st.timer != nil {
		st.timer.Stop()
	}

	// 建立新的timer
	st.timer = time.AfterFunc(time.Until(st.exp), func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		if h.clients[c] != st {
			// 等lock的時候已經被其他goroutine移除了
			return
		}
		if time.Now().Before(st.exp) {
			// 等lock的時候已經被其他goroutine給renew了
			return
		}
		if st.userID == 0 {
			// 等lock的時候已經被其他goroutine降級回匿名了（例如送了壞掉的 auth frame）
			return
		}
		h.downgradeLocked(ctx, c, st, reasonExpired)
		h.sendLocked(ctx, c, st, expired)
	})
}

// refcount +1（呼叫時要持有 h.mu）
func (h *Hub) memberJoinedLocked(userID int64) {
	count := h.members[userID] + 1
	h.members[userID] = count
}

// refcount -1（呼叫時要持有 h.mu）
func (h *Hub) memberLeftLocked(userID int64) {
	count := h.members[userID] - 1
	if count <= 0 {
		delete(h.members, userID)
	} else {
		h.members[userID] = count
	}
}

// member = 不重複 user 數（len(members)）；anonymous = 匿名連線數（直接數 clients，最多 maxClients 條）
func (h *Hub) updateViewersGaugeLocked() {
	anon := 0
	for _, st := range h.clients {
		if st.userID == 0 {
			anon++
		}
	}
	prom.WebsocketViewers.WithLabelValues("member").Set(float64(len(h.members)))
	prom.WebsocketViewers.WithLabelValues("anonymous").Set(float64(anon))
}

func identityLabel(userID int64) string {
	if userID == 0 {
		return anonymous
	}
	return strconv.FormatInt(userID, 10)
}
