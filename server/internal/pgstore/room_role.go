package pgstore

import (
	"context"
	"errors"

	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 讀某個 user 在某間房的角色（房主 / 房管 / 被 ban / 一般觀眾）
// 房間不存在或已刪除 → model.ErrRoomNotFound；user 不存在不會報錯，一樣算一般觀眾
// 被 ban 的人這裡回 RoomRoleBanned：「對外要說成 viewer」是 handler 的事，store 如實回報
func (this *DBStore) GetUserRoomRole(ctx context.Context, roomPublicID string, userID int64) (model.RoomRole, error) {
	var ownerID int64
	var role *string
	err := this.dbPool.QueryRow(ctx,
		`
			SELECT r.owner_id, rr.role
			FROM rooms r
			LEFT JOIN room_roles rr ON rr.room_id = r.id AND rr.user_id = $2
			WHERE r.public_id = $1
				AND r.deleted_at IS NULL
		`, roomPublicID, userID,
	).Scan(&ownerID, &role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", model.ErrRoomNotFound
		}
		return "", err
	}
	return roleOf(userID, ownerID, role), nil
}

// 房主 > 表裡的角色 > 一般觀眾（沒有那一列）
func roleOf(userID, ownerID int64, stored *string) model.RoomRole {
	if userID == ownerID {
		return model.RoomRoleOwner
	}
	if stored == nil {
		return model.RoomRoleViewer
	}
	return model.RoomRole(*stored)
}

// 在一個 transaction 裡處理角色變更：actor 要把 target 改成 desired（Viewer = 移除）
// 用途：PUT / DELETE /rooms/{public_id}/roles/{user_id}
// 順序固定（先看操作者、再看對象是否存在，最後比層級），一般觀眾才無法拿來探測 user 是否存在：
//  1. 鎖房間那一列（FOR NO KEY UPDATE）；房間不存在 / 已刪除 → ErrRoomNotFound
//  2. 查操作者角色；不是房主也不是房管 → Forbidden，不查對象
//  3. 對象不存在 → model.ErrUserNotFound
//  4. 查對象角色和 bannedByOwner，交給 DecideRoleChange
//  5. Allow 才寫入（Viewer = 刪除，其他 = upsert，assigned_by = actor、created_at = now()）；Noop 什麼都不寫
//
// 鎖用 FOR NO KEY UPDATE 而不是 FOR UPDATE：同房間的角色變更排隊，但不擋 messages INSERT 對 rooms 的 FK 檢查
// （FK 檢查拿 FOR KEY SHARE，跟 NO KEY UPDATE 不衝突，跟 FOR UPDATE 會衝突）
func (this *DBStore) ChangeRoomRole(ctx context.Context, roomPublicID string, actorID, targetID int64, desired model.RoomRole) (model.RoleChangeResult, error) {
	tx, err := this.dbPool.Begin(ctx)
	if err != nil {
		return model.RoleChangeResult{}, err
	}
	// 任何 return 都 rollback（也放開房間的鎖）；Commit 之後再 Rollback 是 no-op
	defer tx.Rollback(ctx)

	var roomID, ownerID int64
	err = tx.QueryRow(ctx,
		`
			SELECT id, owner_id
			FROM rooms
			WHERE public_id = $1
				AND deleted_at IS NULL
			FOR NO KEY UPDATE
		`, roomPublicID).Scan(&roomID, &ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.RoleChangeResult{}, model.ErrRoomNotFound
		}
		return model.RoleChangeResult{}, err
	}

	actorRole, _, err := roleInRoom(ctx, tx, roomID, ownerID, actorID)
	if err != nil {
		return model.RoleChangeResult{}, err
	}
	if actorRole != model.RoomRoleOwner && actorRole != model.RoomRoleModerator {
		return model.RoleChangeResult{Decision: model.RoleChangeForbidden}, nil
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, targetID).Scan(&exists); err != nil {
		return model.RoleChangeResult{}, err
	}
	if !exists {
		return model.RoleChangeResult{}, model.ErrUserNotFound
	}

	targetRole, assignedBy, err := roleInRoom(ctx, tx, roomID, ownerID, targetID)
	if err != nil {
		return model.RoleChangeResult{}, err
	}
	// assigned_by 是 NULL（指派者已被刪除）→ 不算房主 ban 的
	bannedByOwner := assignedBy != nil && *assignedBy == ownerID

	decision := model.DecideRoleChange(actorRole, targetRole, desired, bannedByOwner)
	result := model.RoleChangeResult{Decision: decision}
	if decision != model.RoleChangeAllow {
		return result, nil
	}

	if desired == model.RoomRoleViewer {
		_, err = tx.Exec(ctx, `DELETE FROM room_roles WHERE room_id = $1 AND user_id = $2`, roomID, targetID)
	} else {
		_, err = tx.Exec(ctx,
			`
				INSERT INTO room_roles(room_id, user_id, role, assigned_by, created_at)
				VALUES ($1, $2, $3, $4, now())
				ON CONFLICT (room_id, user_id)
				DO UPDATE SET role = EXCLUDED.role, assigned_by = EXCLUDED.assigned_by, created_at = EXCLUDED.created_at
			`, roomID, targetID, string(desired), actorID)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		// 對象在上面檢查之後、寫入之前被刪除：FK 擋下來，當成對象不存在
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "room_roles_user_id_fkey" {
			return model.RoleChangeResult{}, model.ErrUserNotFound
		}
		return model.RoleChangeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.RoleChangeResult{}, err
	}

	result.NewlyBanned = desired == model.RoomRoleBanned && targetRole != model.RoomRoleBanned
	return result, nil
}

// 在 transaction 裡查某個 user 在某間房的角色，順便回傳 assigned_by（沒有那一列或是房主 = nil）
func roleInRoom(ctx context.Context, tx pgx.Tx, roomID, ownerID, userID int64) (model.RoomRole, *int64, error) {
	if userID == ownerID {
		return model.RoomRoleOwner, nil, nil
	}
	var role string
	var assignedBy *int64
	err := tx.QueryRow(ctx,
		`SELECT role, assigned_by FROM room_roles WHERE room_id = $1 AND user_id = $2`,
		roomID, userID).Scan(&role, &assignedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RoomRoleViewer, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return model.RoomRole(role), assignedBy, nil
}

// 列出房間的角色名單（不含房主），最新的在前：created_at DESC, user_id DESC
// after == nil → 第一頁；回傳的 cursor == nil 代表沒有下一頁（同 GetRoomLists：多撈一筆判斷）
// 房間不存在或已刪除 → 空名單（handler 先用 GetUserRoomRole 確認過房間）
func (this *DBStore) ListRoomRoles(ctx context.Context, roomPublicID string, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error) {
	if limit > model.MaxRoomListLimit || limit <= 0 {
		return nil, nil, model.ErrInvalidLimit
	}
	sql := `
		SELECT rr.user_id, u.username, rr.role, rr.assigned_by, ab.username, rr.created_at
		FROM room_roles rr
		JOIN rooms r ON r.id = rr.room_id
		JOIN users u ON u.id = rr.user_id
		LEFT JOIN users ab ON ab.id = rr.assigned_by
		WHERE r.public_id = $1
			AND r.deleted_at IS NULL
	`
	args := []any{roomPublicID, limit + 1}
	if after != nil {
		sql += ` AND (rr.created_at, rr.user_id) < ($3, $4)`
		args = append(args, after.CreatedAt, after.UserID)
	}
	sql += ` ORDER BY rr.created_at DESC, rr.user_id DESC LIMIT $2`

	rows, err := this.dbPool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	entries := []model.RoomRoleEntry{}
	for rows.Next() {
		var e model.RoomRoleEntry
		var role string
		if err := rows.Scan(&e.UserID, &e.Username, &role, &e.AssignedBy, &e.AssignedByUsername, &e.CreatedAt); err != nil {
			return nil, nil, err
		}
		e.Role = model.RoomRole(role)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if len(entries) <= limit {
		return entries, nil, nil
	}
	entries = entries[:limit]
	last := entries[limit-1]
	return entries, &model.RoleCursor{CreatedAt: last.CreatedAt, UserID: last.UserID}, nil
}
