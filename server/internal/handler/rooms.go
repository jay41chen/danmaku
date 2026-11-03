package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/store"
)

type RoomHandler struct {
	db     store.DBStoreInterface
	logger *slog.Logger
}

// constructor
func NewRoomHandler(db store.DBStoreInterface) *RoomHandler {
	return &RoomHandler{
		db:     db,
		logger: slog.Default().With("component", "rooms"),
	}
}

// POST /rooms 的 request body
type createRoomRequest struct {
	RoomName string `json:"room_name"`
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
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// 只檢查、不改寫：存的是原樣（頭尾有空白直接拒絕，不幫忙 trim）
	name := req.RoomName
	if err := validateRoomName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	room, err := this.db.CreateRoom(r.Context(), userID, name)
	if errors.Is(err, store.ErrRoomLimitReached) {
		// 額度是「權限」問題，所以回 403：
		//   409 是「跟現有資料衝突」（例如帳號重複）；422 是「請求內容本身有問題」，這裡請求內容沒錯
		// 目前 HTTP error 都是純文字，client 還分不出 403 的原因，之後要做成結構化錯誤（ROADMAP W5）
		http.Error(w, "room limit reached", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrRoomOwnerNotFound) {
		// user 已刪除但 access token 還沒過期：回跟 RequireAuth 一模一樣的 401
		httpx.WriteUnauthorized(w)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "create room failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusCreated, room)
}

// 公開：知道 public_id 的人都能查
func (this *RoomHandler) HandleGetRoom(w http.ResponseWriter, r *http.Request) {
	room, err := this.db.GetRoomByPublicID(r.Context(), r.PathValue("public_id"))
	if errors.Is(err, store.ErrRoomNotFound) {
		// 已刪除的房間也回 404、不是 410：不讓外人知道「這個房間曾經存在過」
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get room failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	httpx.WriteJSON(w, r, this.logger, http.StatusOK, room)
}
