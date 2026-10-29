package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	middleware "github.com/jay41chen/danmaku/server/internal/middleware"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// 房間名稱長度上限（字元數，不是 bytes）
const maxRoomNameRunes = 30

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
// 檢查 body（trim 後 1..30 字元）
// 存 DB（額度檢查在 store 的 transaction 裡）
// 回 201
func (this *RoomHandler) HandleCreateRoom(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		// 走到這裡是 server 設定錯誤、不是 client 的錯：這個 route 忘了用 post 註冊（誤用 publicPost）
		this.logger.ErrorContext(r.Context(), "no user id in context, route must be registered with post, not publicPost")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var req createRoomRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// 跟訊息不同：房間名稱存的是 trim 後的值（前後空白對名稱沒有意義）
	name := strings.TrimSpace(req.RoomName)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxRoomNameRunes {
		http.Error(w, fmt.Sprintf("room_name must be 1 to %d characters", maxRoomNameRunes), http.StatusBadRequest)
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

	httpx.SetContentType(w)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(room)
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

	httpx.SetContentType(w)
	json.NewEncoder(w).Encode(room)
}
