package model

import (
	"encoding/base64"
	"time"
)

type Room struct {
	ID            int64     `json:"-"` // 內部 PK，不對外
	PublicID      string    `json:"public_id"`
	OwnerID       int64     `json:"-"`
	OwnerUsername string    `json:"owner_username"` // 網址 /{username}/{public_id} 用
	Name          string    `json:"room_name"`
	CreatedAt     time.Time `json:"created_at"`
}

const (
	// migration 000010 建的房間，裝房間功能之前的舊訊息；已移除的 /messages endpoints 以前讀寫的就是它，
	// 現在是 client 的預設房間（client/src/config.ts），test 也會用
	LegacyRoomPublicID = "legacy00000"

	// public_id 用幾個隨機 bytes 產生（8 bytes 編成 base64url 是 11 字元）
	PublicIDBytes = 8
	// 房間列表一頁最多幾筆；handler 解析 query 的 limit 時也用這個，上限只定義在這裡
	MaxRoomListLimit = 50
)

// 房間列表的翻頁位置：上一頁「最後一筆回傳的房間」的排序鍵
// 只放排序用得到的欄位（created_at, public_id），跟 index 的欄位一一對應
// （Kotlin 類比：data class RoomCursor(val createdAt: Instant, val publicId: String)）
type RoomCursor struct {
	CreatedAt time.Time
	PublicID  string
}

// public_id 的格式：PublicIDBytes 個隨機 bytes，用 RawURLEncoding 編碼（跟 store 的 insertRoom 產生的方式一樣）
// 放在 model：格式（PublicIDBytes）只定義在這裡，store 產生、handler 檢查都用它，改格式時兩邊一起變
// 只檢查格式，不查 DB：格式對但不存在的 id，還是要查了才知道
// 給 handler 擋掉明顯錯的 id（page token 裡的 public_id；之後 GET /rooms/{public_id} 也能用，不用打 DB）
// 只看格式、不碰 DB，所以是 model 的函式，不是 store interface 的 method
func ValidPublicID(s string) bool {
	// 1. 長度：base64 每個字元裝 6 bits，8 bytes = 64 bits → 10 個字元還剩 4 bits → 11 個字元（Raw：不補 =）
	//    讓標準庫算，不要寫死 11：PublicIDBytes 改了這裡會跟著變
	if len(s) != base64.RawURLEncoding.EncodedLen(PublicIDBytes) {
		return false
	}
	// 2. 字元：能 decode 成功，代表每個字元都在 A-Z a-z 0-9 - _ 裡面，也沒有 =
	_, err := base64.RawURLEncoding.DecodeString(s)

	return err == nil
}
