package usecase

import (
	"context"
	"fmt"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/textrule"
)

// 這個 use case 需要的 DB 方法（consumer-side interface，同 ADR 0009）
type roomStore interface {
	CreateRoom(ctx context.Context, ownerID int64, name string, maxRooms int) (model.Room, error)
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)
	GetRoomLists(ctx context.Context, userID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error)
	GetRoomsByPublicIDs(ctx context.Context, publicIDs []string) ([]model.Room, error)
}

// 熱門房間排行的讀取（consumer-side interface；實作是 *redisstore.RedisStore）
type viewerRanking interface {
	TopRooms(ctx context.Context, n int) ([]model.RoomViewers, error)
}

// 房間的業務操作：不知道 HTTP 和 SQL
type RoomUseCase struct {
	store   roomStore
	ranking viewerRanking // 不需要熱門列表的 test 可傳 nil（不要呼叫 ListPopularRooms）
}

// constructor
func NewRoomUseCase(store roomStore, ranking viewerRanking) *RoomUseCase {
	return &RoomUseCase{store: store, ranking: ranking}
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

// 目前人數最多的前 limit 間房間（由多到少），帶人數；結果可能少於 limit
// 流程：排行取前 limit 名 → 用 public_id 去 DB 撈房間 → 依排行順序重排，DB 沒回的（已刪除）丟掉
// 排行讀取失敗回 ErrViewerRankingUnavailable（%w 包著原因）；DB 的錯誤原樣回傳
// 用途：GET /rooms?sort=viewers
func (this *RoomUseCase) ListPopularRooms(ctx context.Context, limit int) ([]model.PopularRoom, error) {
	top, err := this.ranking.TopRooms(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrViewerRankingUnavailable, err)
	}
	if len(top) == 0 {
		return []model.PopularRoom{}, nil
	}
	ids := make([]string, len(top))
	for i, rv := range top {
		ids[i] = rv.PublicID
	}
	rooms, err := this.store.GetRoomsByPublicIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.Room, len(rooms))
	for _, r := range rooms {
		byID[r.PublicID] = r
	}
	result := make([]model.PopularRoom, 0, len(top))
	for _, rv := range top {
		room, ok := byID[rv.PublicID]
		if !ok {
			continue // 已刪除的房間
		}
		result = append(result, model.PopularRoom{Room: room, ViewerCount: rv.Count})
	}
	return result, nil
}
