package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jay41chen/danmaku/server/internal/httpx"
	"github.com/jay41chen/danmaku/server/internal/model"
)

// 這個 handler 需要的 DB 方法（為什麼定義在這裡見 authStore）
type roleStore interface {
	ChangeRoomRole(ctx context.Context, roomPublicID string, actorID, targetID int64, desired model.RoomRole) (model.RoleChangeResult, error)
	GetUserRoomRole(ctx context.Context, roomPublicID string, userID int64) (model.RoomRole, error)
	ListRoomRoles(ctx context.Context, roomPublicID string, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error)
}

// 房間角色（房管 / 房間級 shadow ban）的 handler
type RoleHandler struct {
	db     roleStore
	redis  messagePublisher
	logger *slog.Logger
}

// constructor
func NewRoleHandler(db roleStore, redis messagePublisher) *RoleHandler {
	return &RoleHandler{
		db:     db,
		redis:  redis,
		logger: slog.Default().With("component", "room_roles"),
	}
}

// PUT /rooms/{public_id}/roles/{user_id} 的 request body
type setRoleRequest struct {
	Role string `json:"role"`
}

// GET /rooms/{public_id}/roles 的一列；assigned_by 相關欄位在指派者已被刪除時是 null
type roleListItem struct {
	UserID             int64     `json:"user_id"`
	Username           string    `json:"username"`
	Role               string    `json:"role"`
	AssignedBy         *int64    `json:"assigned_by"`
	AssignedByUsername *string   `json:"assigned_by_username"`
	CreatedAt          time.Time `json:"created_at"`
}

// next_page_token 沒有下一頁時整個欄位省略（同 roomListResponse）
type roleListResponse struct {
	Roles         []roleListItem `json:"roles"`
	NextPageToken string         `json:"next_page_token,omitempty"`
}

// GET /rooms/{public_id}/me 的 response
type myRoleResponse struct {
	Role string `json:"role"`
}

// PUT /rooms/{public_id}/roles/{user_id}：把對象指派為房管或 ban 掉（冪等，成功回 204）
// flow：取 user id → 解析 path 的 user_id → 檢查 body → 存 DB（權限和階級規則都在 store 的 transaction 裡）→ 新被 ban 才廣播 user_hidden
func (this *RoleHandler) HandleSetRole(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	targetID, ok := parseTargetUserID(w, r)
	if !ok {
		return
	}

	var req setRoleRequest
	if err := httpx.DecodeJSONBody(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}
	// 只收 moderator / banned：owner 不能被指派，viewer 要用 DELETE
	desired := model.RoomRole(req.Role)
	if desired != model.RoomRoleModerator && desired != model.RoomRoleBanned {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_role", "invalid role")
		return
	}

	this.changeRole(w, r, actorID, targetID, desired)
}

// DELETE /rooms/{public_id}/roles/{user_id}：移除對象的角色（撤銷房管或解除 ban），成功回 204
func (this *RoleHandler) HandleDeleteRole(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	targetID, ok := parseTargetUserID(w, r)
	if !ok {
		return
	}
	this.changeRole(w, r, actorID, targetID, model.RoomRoleViewer)
}

// 解析 path 的 user_id；不是正整數 → 404 user_not_found（回傳 false，已經回應）
// 跟「查無此人」同一個回應：格式錯的 id 本來就不可能存在
func parseTargetUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteError(w, http.StatusNotFound, "user_not_found", "user not found")
		return 0, false
	}
	return id, true
}

// PUT / DELETE 共用：呼叫 store 並把結果翻成 HTTP 回應
// 廣播在 DB commit 之後；失敗只記 log（publishEnvelope），仍回 204，不重試
func (this *RoleHandler) changeRole(w http.ResponseWriter, r *http.Request, actorID, targetID int64, desired model.RoomRole) {
	roomPublicID := r.PathValue("public_id")
	result, err := this.db.ChangeRoomRole(r.Context(), roomPublicID, actorID, targetID, desired)
	if errors.Is(err, model.ErrRoomNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	if errors.Is(err, model.ErrUserNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "user_not_found", "user not found")
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "change room role failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	switch result.Decision {
	case model.RoleChangeForbidden:
		httpx.WriteError(w, http.StatusForbidden, "room_forbidden", "forbidden")
		return
	case model.RoleChangeTargetForbidden:
		httpx.WriteError(w, http.StatusForbidden, "room_target_forbidden", "cannot manage this user")
		return
	case model.RoleChangeConflict:
		httpx.WriteError(w, http.StatusConflict, "room_role_conflict", "unban the user before assigning a role")
		return
	case model.RoleChangeAllow, model.RoleChangeNoop:
		// 往下回 204
	default:
		// DecideRoleChange 新增了這裡沒處理的結果：fail closed
		this.logger.ErrorContext(r.Context(), "unknown role decision", "decision", int(result.Decision))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	if result.NewlyBanned {
		publishEnvelope(r.Context(), this.logger, this.redis, roomPublicID, userHiddenEnvelope(targetID))
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /rooms/{public_id}/me：我在這間房的角色（owner / moderator / viewer）
// 被 ban 的人也回 viewer：shadow ban 才藏得住。回應看是誰在問，所以 no-store（放最前面，錯誤回應也不該被快取）
func (this *RoleHandler) HandleMyRole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	role, ok := this.lookupRole(w, r, userID)
	if !ok {
		return
	}
	if role == model.RoomRoleBanned {
		role = model.RoomRoleViewer
	}
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, myRoleResponse{Role: string(role)})
}

// GET /rooms/{public_id}/roles：角色名單，只有房主和房管看得到
// 被 ban 的人和一般觀眾的回應完全一樣（403 room_forbidden），才不會因此暴露被 ban
func (this *RoleHandler) HandleListRoles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	// 先驗 limit / page_token 再查 DB（同 listRooms）
	limit, ok := parseListLimit(w, r)
	if !ok {
		return
	}
	var after *model.RoleCursor
	if raw := r.URL.Query().Get("page_token"); raw != "" {
		cursor, err := decodeRoleToken(raw)
		if err != nil {
			httpx.WriteCodedError(w, r, this.logger, http.StatusBadRequest, errInvalidPageToken)
			return
		}
		after = &cursor
	}

	role, ok := this.lookupRole(w, r, userID)
	if !ok {
		return
	}
	if role != model.RoomRoleOwner && role != model.RoomRoleModerator {
		httpx.WriteError(w, http.StatusForbidden, "room_forbidden", "forbidden")
		return
	}

	entries, next, err := this.db.ListRoomRoles(r.Context(), r.PathValue("public_id"), after, limit)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "list room roles failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// make 而不是 var：沒有資料時 JSON 要是 []、不是 null
	resp := roleListResponse{Roles: make([]roleListItem, 0, len(entries))}
	for _, e := range entries {
		resp.Roles = append(resp.Roles, roleListItem{
			UserID:             e.UserID,
			Username:           e.Username,
			Role:               string(e.Role),
			AssignedBy:         e.AssignedBy,
			AssignedByUsername: e.AssignedByUsername,
			CreatedAt:          e.CreatedAt,
		})
	}
	if next != nil {
		token, err := encodeRoleToken(*next)
		if err != nil {
			this.logger.ErrorContext(r.Context(), "encode page token failed", "error", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		resp.NextPageToken = token
	}
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, resp)
}

// 查 userID 在 path 指定房間的角色；房間不存在 → 404 room_not_found，其他錯誤 → 500（回傳 false = 已經回應）
func (this *RoleHandler) lookupRole(w http.ResponseWriter, r *http.Request, userID int64) (model.RoomRole, bool) {
	role, err := this.db.GetUserRoomRole(r.Context(), r.PathValue("public_id"), userID)
	if errors.Is(err, model.ErrRoomNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
		return "", false
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get room role failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return "", false
	}
	return role, true
}
