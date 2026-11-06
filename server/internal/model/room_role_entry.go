package model

import "time"

// 房間角色名單裡的一列（GET /rooms/{public_id}/roles）
// AssignedBy / AssignedByUsername 用指標：assigned_by 是 NULL（指派者已被刪除）時序列化成 null
type RoomRoleEntry struct {
	UserID             int64
	Username           string
	Role               RoomRole
	AssignedBy         *int64
	AssignedByUsername *string
	CreatedAt          time.Time
}

// 角色名單的翻頁位置：上一頁最後一筆的排序鍵（created_at, user_id），跟 ORDER BY 一一對應
type RoleCursor struct {
	CreatedAt time.Time
	UserID    int64
}

// ChangeRoomRole 的結果：Decision 是 DecideRoleChange 的判斷；
// NewlyBanned = 這次讓對象「新」變成被 ban（原本不是 banned、現在是），handler 依此廣播 user_hidden
// 房主對已被 ban 的人再 ban 一次（接手 assigned_by）不算 NewlyBanned
type RoleChangeResult struct {
	Decision    RoleDecision
	NewlyBanned bool
}
