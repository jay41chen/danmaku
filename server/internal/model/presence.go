package model

// 跨 server 房間在線人數（presence）的一筆資料：某條連線現在在哪個房間、是誰
// 放在 model 是因為 ws（Hub 產生快照）和 redisstore（寫進 Redis）都要用，兩邊不用互相 import
type PresenceEntry struct {
	Room   string // 房間 public_id
	ConnID string // 連線 ID（ws.Client.ID），整個叢集唯一
	UserID int64  // 0 = 匿名
}
