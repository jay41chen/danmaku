package usecase

import (
	"context"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/textrule"
)

// 這個 use case 需要的 DB 方法（consumer-side interface，同 ADR 0009）
type roomStore interface {
	CreateRoom(ctx context.Context, ownerID int64, name string, maxRooms int) (model.Room, error)
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)
	GetRoomLists(ctx context.Context, userID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error)
}

// 房間的業務操作：不知道 HTTP 和 SQL
type RoomUseCase struct {
	store roomStore
}

// constructor
func NewRoomUseCase(store roomStore) *RoomUseCase {
	return &RoomUseCase{store: store}
}

// 建房間：先檢查房名（textrule.RoomName，只檢查、不改寫），再交給 store 建（額度由 store 在 transaction 裡檢查）
// 房名不合規範回 *model.CodedError，不碰 store；store 的 error 原樣回傳
// 用途：POST /rooms
func (this *RoomUseCase) CreateRoom(ctx context.Context, ownerID int64, name string) (model.Room, error) {
	if err := textrule.RoomName(name); err != nil {
		return model.Room{}, err
	}
	return this.store.CreateRoom(ctx, ownerID, name, model.MaxRoomsPerUser)
}

// 用 public_id 查一個房間
// 目前是 pass-through：刻意讓 handler 不直接碰 store，之後加規則（例如可見度）放這裡
// 用途：GET /rooms/{public_id}
func (this *RoomUseCase) GetRoom(ctx context.Context, publicID string) (model.Room, error) {
	return this.store.GetRoomByPublicID(ctx, publicID)
}

// 列出房間；ownerID 為 nil = 不限 owner
// 目前是 pass-through：刻意讓 handler 不直接碰 store，之後加規則放這裡
// 用途：GET /rooms、GET /me/rooms
func (this *RoomUseCase) ListRooms(ctx context.Context, ownerID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error) {
	return this.store.GetRoomLists(ctx, ownerID, after, limit)
}
