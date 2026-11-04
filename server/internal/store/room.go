package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/randx"

	"github.com/jackc/pgx/v5"
)

var (
	// 查不到、或已 soft delete：對外都當成沒這個房間
	ErrRoomNotFound = errors.New("room not found")
	// 這個 user 的未刪除房間已達上限
	ErrRoomLimitReached = errors.New("room limit reached")
	// owner 已被刪除，但他的 access token 還沒過期（同 ErrMessageUserNotFound 的情況）
	ErrRoomOwnerNotFound = errors.New("room owner not found")
	// 房間列表的 limit 不在 1..MaxRoomListLimit：handler 應該先擋掉，走到這裡代表 caller 有 bug
	ErrInvalidLimit = errors.New("invalid limit")
)

const (
	// migration 000010 建的房間，裝房間功能之前的舊訊息；已移除的 /messages endpoints 以前讀寫的就是它，
	// 現在是 client 的預設房間（client/src/config.ts），test 也會用
	LegacyRoomPublicID = "legacy00000"

	// 每個 user 最多同時擁有的（未刪除）房間數
	maxRoomsPerUser = 3
	// public_id 用幾個隨機 bytes 產生（8 bytes 編成 base64url 是 11 字元）
	publicIDBytes = 8
	// public_id 撞號時最多試幾次：64 bits 下撞一次已經極罕見，連撞 3 次幾乎不可能，真的發生就當成錯誤
	maxPublicIDAttempts = 3
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

// 所有「讀房間」的路徑都要經過這類 method，deleted_at IS NULL 的條件才不會被漏掉
func (this *DBStore) GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error) {
	var room model.Room
	err := this.dbPool.QueryRow(ctx,
		`
			SELECT r.id, r.public_id, r.owner_id, u.username, r.room_name, r.created_at
			FROM rooms r
			JOIN users u ON u.id = r.owner_id
			WHERE r.public_id = $1
				AND r.deleted_at IS NULL
		`, publicID,
	).Scan(&room.ID, &room.PublicID, &room.OwnerID, &room.OwnerUsername, &room.Name, &room.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Room{}, ErrRoomNotFound
		}
		return model.Room{}, err
	}
	return room, nil
}

// 數 owner 目前未刪除的房間數（soft delete 的不算額度）
// 不用再加 FOR UPDATE：lockUser 已經讓同一個 owner 的請求排隊；
// 預設 Read Committed 下每句 statement 都拿新的 snapshot，所以看得到前一個請求剛 commit 的房間
// （而且 count(*) 這種聚合函式本來就不能 FOR UPDATE）
func countActiveRooms(ctx context.Context, tx pgx.Tx, ownerID int64) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		`
			SELECT count(*)
			FROM rooms
			WHERE owner_id = $1
				AND deleted_at IS NULL
		`, ownerID).Scan(&count)
	// count(*) 一定回一列，不會有 ErrNoRows；其他錯誤（斷線等）一定要往上回，
	// 吞掉的話 count 停在 0 → 上限檢查直接放行（fail open）
	if err != nil {
		return 0, err
	}
	return count, nil
}

// INSERT 一個新房間，public_id 撞號時換一個重試，最多 maxPublicIDAttempts 次
// 只負責寫入，不 Commit：transaction 什麼時候結束由 CreateRoom 決定
// 回傳的 Room 沒有 OwnerUsername（那是 lockUser 拿到的，由 CreateRoom 補上）
func insertRoom(ctx context.Context, tx pgx.Tx, ownerID int64, name string) (model.Room, error) {
	for range maxPublicIDAttempts {
		// public_id：跟 refresh token 共用 randx.URLText（crypto/rand，猜不到）
		// base64 是每 6 bits 直接對應一個字元，沒有取模，所以沒有 modulo bias；
		// 8 bytes = 64 bits 的熵，編碼後 11 字元（64 = 10×6 + 4，最後一個字元只帶 4 bits），跟 YouTube 的 video id 一樣
		// 每圈都產生新的：撞號時 continue 才會換一個 id 重試
		publicID := randx.URLText(publicIDBytes)

		var room model.Room
		// ON CONFLICT DO NOTHING：撞號時不報錯、RETURNING 回 0 列 → Scan 得到 pgx.ErrNoRows（下面靠這個判斷撞號）
		// 不讓它報 unique violation：transaction 裡任何一句失敗，整個 tx 會變成 aborted，之後每句都被拒絕、沒辦法重試
		err := tx.QueryRow(ctx,
			`
				INSERT INTO rooms(public_id, room_name, owner_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (public_id) DO NOTHING
				RETURNING id, public_id, room_name, owner_id, created_at
			`, publicID, name, ownerID).Scan(&room.ID, &room.PublicID, &room.Name, &room.OwnerID, &room.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // 撞號，換一個 id 再試
		}
		if err != nil {
			return model.Room{}, err
		}
		return room, nil
	}
	// 迴圈跑完 = 每次都撞號（成功的路徑在迴圈裡就 return 了）
	return model.Room{}, fmt.Errorf("public_id collided %d times", maxPublicIDAttempts)
}

func (this *DBStore) CreateRoom(ctx context.Context, ownerID int64, name string) (model.Room, error) {
	tx, err := this.dbPool.Begin(ctx)
	if err != nil {
		return model.Room{}, err
	}
	// 中途任何一個 return 都會 rollback（也會放開 lockUser 的鎖）；Commit 成功之後再 Rollback 不會做任何事
	// （回傳的 ErrTxClosed 直接忽略），所以不用每個 return 前都手動 rollback
	defer tx.Rollback(ctx)
	username, err := lockUser(ctx, tx, ownerID)
	if err != nil {
		// user 已刪除、但 access token 還沒過期：對 room 這一層來說是「owner 不存在」
		if errors.Is(err, ErrUserNotFound) {
			return model.Room{}, ErrRoomOwnerNotFound
		}
		return model.Room{}, err
	}
	count, err := countActiveRooms(ctx, tx, ownerID)
	if err != nil {
		return model.Room{}, err
	}
	if count >= maxRoomsPerUser {
		return model.Room{}, ErrRoomLimitReached
	}
	room, err := insertRoom(ctx, tx, ownerID, name)
	if err != nil {
		return model.Room{}, err
	}
	// lock → count → insert 全部成功才 Commit：三步一起生效，同時放開 lockUser 的鎖
	if err := tx.Commit(ctx); err != nil {
		return model.Room{}, err
	}
	room.OwnerUsername = username
	return room, nil
}

// 列出未刪除的房間（cursor pagination），排序 created_at DESC, public_id DESC
// userID == nil → 所有人的房間（GET /rooms）；不是 nil → 只看這個 owner 的（GET /me/rooms）
// after == nil → 第一頁；不是 nil → 從 after 之後開始
// 回傳的 cursor == nil 代表沒有下一頁
// 回傳的 Room 沒有 OwnerUsername（沒 JOIN users）：handler 要用只含 room_name / public_id 的 DTO 回傳
func (this *DBStore) GetRoomLists(ctx context.Context, userID *int64, after *RoomCursor, limit int) ([]model.Room, *RoomCursor, error) {
	// handler 應該已經驗證過；store 再擋一次：limit = 0 時下面的 rooms[limit-1] 會 panic，
	// 負數會讓 SQL 變成 LIMIT 負數，Postgres 直接報錯
	if limit > MaxRoomListLimit || limit <= 0 {
		return nil, nil, ErrInvalidLimit
	}
	var (
		rooms  []model.Room
		err    error
		cursor *RoomCursor
	)

	// 列出未刪除的房間，排序 created_at DESC, public_id DESC
	// 注意這邊要傳入limit+1，用來[確定是否還有下一頁]
	if userID == nil {
		rooms, err = this.getPublicRoomLists(ctx, after, limit+1)
	} else {
		rooms, err = this.getUserRoomLists(ctx, *userID, after, limit+1)
	}
	if err != nil {
		return nil, nil, err
	}

	if len(rooms) <= limit {
		// 沒有下一頁
		cursor = nil
	} else {
		// 有下一頁
		// 先把rooms整理好，多拿的一筆去掉
		rooms = rooms[:limit]
		// 再去拿現在rooms的最後一筆
		// 不是多拿的那筆：下一頁的條件是 < cursor，用多拿的那筆的話它自己會被跳過、永遠不會出現
		r := rooms[limit-1]
		cursor = &RoomCursor{CreatedAt: r.CreatedAt, PublicID: r.PublicID}
	}

	return rooms, cursor, nil
}

// GET /rooms 用：所有人的未刪除房間
// n 是要撈幾筆；翻頁規則（+1、算 cursor）由 GetRoomLists 處理，這裡不用知道
func (this *DBStore) getPublicRoomLists(ctx context.Context, after *RoomCursor, n int) ([]model.Room, error) {
	var (
		sql  string
		args []any
	)
	if after == nil {
		// 第一頁
		sql = `
			SELECT id, public_id, owner_id, room_name, created_at
			FROM rooms
			WHERE deleted_at IS NULL
			ORDER BY created_at DESC, public_id DESC
			LIMIT $1
			`
		args = append(args, n)
	} else {
		sql = `
			SELECT id, public_id, owner_id, room_name, created_at
			FROM rooms
			WHERE deleted_at IS NULL 
				AND (created_at, public_id) < ($2, $3)
			ORDER BY created_at DESC, public_id DESC
			LIMIT $1
			`
		args = append(args, n, after.CreatedAt, after.PublicID)
	}

	return this.queryRooms(ctx, sql, args...)
}

// GET /me/rooms 用：只看 ownerID 的未刪除房間
func (this *DBStore) getUserRoomLists(ctx context.Context, ownerID int64, after *RoomCursor, n int) ([]model.Room, error) {
	var (
		sql  string
		args []any
	)
	if after == nil {
		// 第一頁
		sql = `
			SELECT id, public_id, owner_id, room_name, created_at
			FROM rooms
			WHERE deleted_at IS NULL
				AND owner_id = $1
			ORDER BY created_at DESC, public_id DESC
			LIMIT $2
			`
		args = append(args, ownerID, n)
	} else {
		sql = `
			SELECT id, public_id, owner_id, room_name, created_at
			FROM rooms 
			WHERE deleted_at IS NULL 
				AND owner_id = $1
				AND (created_at, public_id) < ($3, $4)
			ORDER BY created_at DESC, public_id DESC
			LIMIT $2
			`
		args = append(args, ownerID, n, after.CreatedAt, after.PublicID)
	}

	return this.queryRooms(ctx, sql, args...)
}

// 兩個列表共用：執行查詢，把結果掃成 []model.Room
// SELECT 的欄位跟順序要一致：id, public_id, owner_id, room_name, created_at
func (this *DBStore) queryRooms(ctx context.Context, sql string, args ...any) ([]model.Room, error) {
	// 特別注意這邊要傳入的是`args...`而不是`args`
	rows, err := this.dbPool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rooms := []model.Room{}
	for rows.Next() {
		var room model.Room
		if err := rows.Scan(&room.ID, &room.PublicID, &room.OwnerID, &room.Name, &room.CreatedAt); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rooms, nil
}

// public_id 的格式：publicIDBytes 個隨機 bytes，用 RawURLEncoding 編碼（跟 insertRoom 產生的方式一樣）
// 放在 store：格式是這裡定義的（publicIDBytes），產生和檢查放一起，改格式時兩邊一起看得到
// 只檢查格式，不查 DB：格式對但不存在的 id，還是要查了才知道
// 給 handler 擋掉明顯錯的 id（page token 裡的 public_id；之後 GET /rooms/{public_id} 也能用，不用打 DB）
// 是 package 層級的函式、不是 DBStore 的 method（不碰 DB），所以不放進 DBStoreInterface
func ValidPublicID(s string) bool {
	// 1. 長度：base64 每個字元裝 6 bits，8 bytes = 64 bits → 10 個字元還剩 4 bits → 11 個字元（Raw：不補 =）
	//    讓標準庫算，不要寫死 11：publicIDBytes 改了這裡會跟著變
	if len(s) != base64.RawURLEncoding.EncodedLen(publicIDBytes) {
		return false
	}
	// 2. 字元：能 decode 成功，代表每個字元都在 A-Z a-z 0-9 - _ 裡面，也沒有 =
	_, err := base64.RawURLEncoding.DecodeString(s)

	return err == nil
}
