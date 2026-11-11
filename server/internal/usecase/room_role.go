package usecase

import (
	"context"
	"fmt"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 這個 use case 需要的 DB 方法（consumer-side interface，同 ADR 0009）
type roleStore interface {
	ChangeRoomRole(ctx context.Context, roomPublicID string, actorID, targetID int64, desired model.RoomRole, decide model.RoleDecider) (model.RoleChangeResult, error)
	GetUserRoomRole(ctx context.Context, roomPublicID string, userID int64) (model.RoomRole, error)
	ListRoomRoles(ctx context.Context, roomPublicID string, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error)
}

// 通知房間裡的連線；訊息格式和發送失敗的處理都是實作方的事，所以沒有回傳 error
type roomNotifier interface {
	NotifyUserHidden(ctx context.Context, roomPublicID string, userID int64)
}

// 房間角色的業務操作：不知道 HTTP、SQL 和 WS 訊息格式
type RoleUseCase struct {
	store    roleStore
	notifier roomNotifier
}

// constructor
func NewRoleUseCase(store roleStore, notifier roomNotifier) *RoleUseCase {
	return &RoleUseCase{store: store, notifier: notifier}
}

// 把對象改成 desired：moderator、banned，或 viewer（撤銷房管、解除 ban），可重送
// 判斷結果翻成 error，新被 ban 才通知
// 用途：PUT / DELETE /rooms/{public_id}/roles/{user_id}
func (this *RoleUseCase) ChangeRole(ctx context.Context, roomPublicID string, actorID, targetID int64, desired model.RoomRole) error {
	if !desired.Assignable() {
		return model.ErrInvalidRole
	}
	result, err := this.store.ChangeRoomRole(ctx, roomPublicID, actorID, targetID, desired, model.DecideRoleChange)
	if err != nil {
		return err
	}
	switch result.Decision {
	case model.RoleChangeForbidden:
		return model.ErrRoomForbidden
	case model.RoleChangeTargetForbidden:
		return model.ErrRoomTargetForbidden
	case model.RoleChangeConflict:
		return model.ErrRoomRoleConflict
	case model.RoleChangeAllow, model.RoleChangeNoop:
		// store 已經 commit，不用再做什麼，往下走
	default:
		return fmt.Errorf("unknown role decision: %d", result.Decision)
	}

	// 新被 ban 才廣播 user_hidden，讓線上觀眾把他的彈幕藏起來
	// 重複 ban、解除 ban 都不發
	if result.NewlyBanned {
		this.notifier.NotifyUserHidden(ctx, roomPublicID, targetID)
	}
	return nil
}

// 查自己在這間房的角色；被 ban 也回 viewer，shadow ban 才藏得住
// 用途：GET /rooms/{public_id}/me
func (this *RoleUseCase) MyRole(ctx context.Context, roomPublicID string, userID int64) (model.RoomRole, error) {
	myRole, err := this.store.GetUserRoomRole(ctx, roomPublicID, userID)
	if err != nil {
		return "", err
	}
	// 被 ban 也回 viewer，shadow ban 才藏得住
	if myRole == model.RoomRoleBanned {
		return model.RoomRoleViewer, nil
	}
	return myRole, nil
}

// 列出角色名單，只有房主和房管看得到；被 ban 的人和一般觀眾得到同樣的錯誤
// 用途：GET /rooms/{public_id}/roles
func (this *RoleUseCase) ListRoles(ctx context.Context, roomPublicID string, viewerID int64, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error) {
	viewerRole, err := this.store.GetUserRoomRole(ctx, roomPublicID, viewerID)
	if err != nil {
		return nil, nil, err
	}
	if viewerRole != model.RoomRoleOwner && viewerRole != model.RoomRoleModerator {
		// 不能使用這功能
		return nil, nil, model.ErrRoomForbidden
	}
	return this.store.ListRoomRoles(ctx, roomPublicID, after, limit)
}
