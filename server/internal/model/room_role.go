package model

// 某人在某間房的角色；房主來自 rooms.owner_id，沒有 room_roles 那一列就是 Viewer
type RoomRole string

const (
	RoomRoleOwner     RoomRole = "owner"
	RoomRoleModerator RoomRole = "moderator"
	RoomRoleBanned    RoomRole = "banned"
	RoomRoleViewer    RoomRole = "viewer"
)

// 角色變更的判斷結果，handler 依此決定要寫 DB，還是回哪種錯誤
type RoleDecision int

const (
	RoleChangeAllow           RoleDecision = iota // 要寫入 DB
	RoleChangeNoop                                // 不用寫入，回 204（重送也安全）
	RoleChangeForbidden                           // 403 room_forbidden：操作者沒有這個權限
	RoleChangeTargetForbidden                     // 403 room_target_forbidden：有權限，但不能動這個對象
	RoleChangeConflict                            // 409 room_role_conflict：要先解除 ban 才能指派
)

// 判斷 actor 能不能把 target 目前的角色改成 desired，集中房間的階級規則：房主 > 房管 > 一般觀眾
// 用途：handler 查完 DB 後呼叫；desired 是 Viewer 代表移除（DELETE）。
// bannedByOwner 只有 target 是 Banned 時才有意義：房主 ban 的，房管不能解除
func DecideRoleChange(actor, target, desired RoomRole, bannedByOwner bool) RoleDecision {
	// fail closed：沒預期到的值一律拒絕，否則會一路走到最後的 Allow
	// handler 也會先擋 desired，這裡多一道是因為放行的代價太高（例如把別人變成房主）
	if desired != RoomRoleModerator && desired != RoomRoleBanned && desired != RoomRoleViewer {
		return RoleChangeForbidden
	}

	// step1：有沒有資格做這件事
	// 用白名單：不是房主也不是房管就拒絕，被 ban 的房管（已是 Banned）也會在這裡被擋
	if actor != RoomRoleOwner && actor != RoomRoleModerator {
		return RoleChangeForbidden
	}
	// 房主不能被任何人動，包括自己；房管動自己則由下面「對象是房管」擋住
	if target == RoomRoleOwner {
		return RoleChangeTargetForbidden
	}
	if actor == RoomRoleModerator {
		// 指派房管只有房主能做，所以先於對象檢查：不管對象是誰都是 Forbidden
		if desired == RoomRoleModerator {
			return RoleChangeForbidden
		}
		// 只能上對下，不能平行管理
		if target == RoomRoleModerator {
			return RoleChangeTargetForbidden
		}
		// 房管不能推翻房主的決定；再 ban 一次則不算推翻，交給 step2 回 Noop
		if desired == RoomRoleViewer && target == RoomRoleBanned && bannedByOwner {
			return RoleChangeTargetForbidden
		}
	}

	// step2：這個變更合不合理
	if target == desired {
		// 房主重複 ban 要寫入：把 assigned_by 改成房主，房管之後才解不開
		if actor == RoomRoleOwner && desired == RoomRoleBanned {
			return RoleChangeAllow
		}
		// 其他重送都不用動；房管重複 ban 也走這裡，才不會覆蓋房主的 assigned_by
		return RoleChangeNoop
	}
	// 指派不能順便解除 ban，要明確先解除
	if desired == RoomRoleModerator && target == RoomRoleBanned {
		return RoleChangeConflict
	}
	return RoleChangeAllow
}
