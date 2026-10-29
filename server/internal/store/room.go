package store

import (
	"context"
	"errors"
	"fmt"

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
)

const (
	// 每個 user 最多同時擁有的（未刪除）房間數
	maxRoomsPerUser = 3
	// public_id 用幾個隨機 bytes 產生（8 bytes 編成 base64url 是 11 字元）
	publicIDBytes = 8
	// public_id 撞號時最多試幾次：64 bits 下撞一次已經極罕見，連撞 3 次幾乎不可能，真的發生就當成錯誤
	maxPublicIDAttempts = 3
)

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

// 一次做三件事：
//  1. 確認 owner 還在（user 已刪除、但 access token 還沒過期）→ ErrRoomOwnerNotFound
//  2. 鎖住 owner 這一列直到 transaction 結束：同一個 user 同時建房會在這裡排隊，
//     後面的 count 跟 INSERT 之間不會被插隊（否則兩個請求都數到 2、都建成功 → 4 間）；不同 user 不會互相卡住
//  3. 拿 username 放進回傳的 Room
//
// 參數是 pgx.Tx 不是 pool：FOR UPDATE 離開 transaction 就沒效（單句自動 commit、鎖馬上放開），讓 compiler 擋掉「拿 pool 來呼叫」
func lockOwner(ctx context.Context, tx pgx.Tx, ownerID int64) (string, error) {
	var username string
	err := tx.QueryRow(ctx,
		`
			SELECT username
			FROM users
			WHERE id = $1
			FOR UPDATE
		`, ownerID).Scan(&username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrRoomOwnerNotFound
		}
		return "", err
	}
	return username, nil
}

// 數 owner 目前未刪除的房間數（soft delete 的不算額度）
// 不用再加 FOR UPDATE：lockOwner 已經讓同一個 owner 的請求排隊；
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
// 回傳的 Room 沒有 OwnerUsername（那是 lockOwner 拿到的，由 CreateRoom 補上）
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
	// 中途任何一個 return 都會 rollback（也會放開 lockOwner 的鎖）；Commit 成功之後再 Rollback 不會做任何事
	// （回傳的 ErrTxClosed 直接忽略），所以不用每個 return 前都手動 rollback
	defer tx.Rollback(ctx)
	username, err := lockOwner(ctx, tx, ownerID)
	if err != nil {
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
	// lock → count → insert 全部成功才 Commit：三步一起生效，同時放開 lockOwner 的鎖
	if err := tx.Commit(ctx); err != nil {
		return model.Room{}, err
	}
	room.OwnerUsername = username
	return room, nil
}
