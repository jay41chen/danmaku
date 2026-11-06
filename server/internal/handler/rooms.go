package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// limit 沒帶時的預設值；上限是 model.MaxRoomListLimit
const defaultRoomListLimit = 20

// 這個 handler 需要的 DB 方法（為什麼定義在這裡見 authStore）
type roomStore interface {
	CreateRoom(ctx context.Context, ownerID int64, name string) (model.Room, error)
	GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error)

	GetRoomLists(ctx context.Context, userID *int64, after *model.RoomCursor, limit int) ([]model.Room, *model.RoomCursor, error)
}

type RoomHandler struct {
	db     roomStore
	logger *slog.Logger
}

// constructor
func NewRoomHandler(db roomStore) *RoomHandler {
	return &RoomHandler{
		db:     db,
		logger: slog.Default().With("component", "rooms"),
	}
}

// POST /rooms 的 request body
type createRoomRequest struct {
	RoomName string `json:"room_name"`
}

// 列表裡每個房間只給這兩個欄位
// 不直接序列化 model.Room：store 的列表沒有 JOIN users，owner_username 是空的，
// 直接輸出會多出空的 owner_username 和 created_at
type roomListItem struct {
	RoomName string `json:"room_name"`
	PublicID string `json:"public_id"`
}

// next_page_token 沒有下一頁時整個欄位省略
type roomListResponse struct {
	Rooms         []roomListItem `json:"rooms"`
	NextPageToken string         `json:"next_page_token,omitempty"`
}

// flow:
// 取 user id（RequireAuth 放的）
// 檢查 body（validateRoomName：1..30 字元、字元規則、頭尾空白直接拒絕，不 trim）
// 存 DB（額度檢查在 store 的 transaction 裡）
// 回 201
func (this *RoomHandler) HandleCreateRoom(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}

	var req createRoomRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}

	// 只檢查、不改寫：存的是原樣（頭尾有空白直接拒絕，不幫忙 trim）
	name := req.RoomName
	if err := validateRoomName(name); err != nil {
		httpx.WriteCodedError(w, r, this.logger, http.StatusBadRequest, err)
		return
	}

	room, err := this.db.CreateRoom(r.Context(), userID, name)
	if errors.Is(err, model.ErrRoomLimitReached) {
		// 額度是「權限」問題，所以回 403：
		//   409 是「跟現有資料衝突」（例如帳號重複）；422 是「請求內容本身有問題」，這裡請求內容沒錯
		// 403 的原因由 body 的 code（room_limit_reached）告訴 client
		httpx.WriteError(w, http.StatusForbidden, "room_limit_reached", "room limit reached")
		return
	}
	if errors.Is(err, model.ErrRoomOwnerNotFound) {
		// user 已刪除但 access token 還沒過期：回跟 RequireAuth 一模一樣的 401
		httpx.WriteUnauthorized(w)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "create room failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, room)
}

// 公開：知道 public_id 的人都能查
func (this *RoomHandler) HandleGetRoom(w http.ResponseWriter, r *http.Request) {
	room, err := this.db.GetRoomByPublicID(r.Context(), r.PathValue("public_id"))
	if errors.Is(err, model.ErrRoomNotFound) {
		// 已刪除的房間也回 404、不是 410：不讓外人知道「這個房間曾經存在過」
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get room failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, room)
}

// GET /rooms：公開，列出所有人的房間
func (this *RoomHandler) HandleListRooms(w http.ResponseWriter, r *http.Request) {
	this.listRooms(w, r, nil)
}

// GET /me/rooms：要登入，只列出自己的房間
func (this *RoomHandler) HandleListMyRooms(w http.ResponseWriter, r *http.Request) {
	// 跟 HandleMe 一樣：回應看是誰在問，所以 CDN / 瀏覽器都不能存
	// 放在最前面：錯誤回應也不該被快取
	w.Header().Set("Cache-Control", "no-store")

	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	this.listRooms(w, r, &userID)
}

// 兩個列表 endpoint 共用；userID 為 nil = 不限 owner
// 驗證順序：先 limit、再 page_token；任何一個不合格都不碰 DB
func (this *RoomHandler) listRooms(w http.ResponseWriter, r *http.Request, userID *int64) {
	query := r.URL.Query()

	limit, ok := parseListLimit(w, r)
	if !ok {
		return
	}

	// 沒帶或空字串 = 第一頁
	var after *model.RoomCursor
	if raw := query.Get("page_token"); raw != "" {
		cursor, err := decodePageToken(raw)
		if err != nil {
			// 不說是哪一項壞掉
			httpx.WriteCodedError(w, r, this.logger, http.StatusBadRequest, errInvalidPageToken)
			return
		}
		after = &cursor
	}

	rooms, next, err := this.db.GetRoomLists(r.Context(), userID, after, limit)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "list rooms failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// make 而不是 var：沒有資料時 JSON 要是 []、不是 null
	resp := roomListResponse{Rooms: make([]roomListItem, 0, len(rooms))}
	for _, room := range rooms {
		resp.Rooms = append(resp.Rooms, roomListItem{RoomName: room.Name, PublicID: room.PublicID})
	}
	if next != nil {
		token, err := encodePageToken(*next)
		if err != nil {
			this.logger.ErrorContext(r.Context(), "encode page token failed", "error", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		resp.NextPageToken = token
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, resp)
}

// 列表 endpoint 共用：解析 query 的 limit（房間列表、角色名單都用）
// 沒帶才用預設值；帶了就一定要合法（?limit= 空字串也算帶了 → 400 invalid_limit）
// 回傳 false = 已經回了 400，呼叫端直接 return
func parseListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := defaultRoomListLimit
	if r.URL.Query().Has("limit") {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n < 1 || n > model.MaxRoomListLimit {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_limit", "invalid limit")
			return 0, false
		}
		limit = n
	}
	return limit, true
}
