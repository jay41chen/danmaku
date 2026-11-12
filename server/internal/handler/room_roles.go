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

// 這個 handler 需要的 use case 方法（為什麼定義在這裡見 authStore）
type roleUseCase interface {
	ChangeRole(ctx context.Context, roomPublicID string, actorID, targetID int64, desired model.RoomRole) error
	MyRole(ctx context.Context, roomPublicID string, userID int64) (model.RoomRole, error)
	ListRoles(ctx context.Context, roomPublicID string, viewerID int64, after *model.RoleCursor, limit int) ([]model.RoomRoleEntry, *model.RoleCursor, error)
}

// 房間角色（房管 / 房間級 shadow ban）的 handler：只處理 HTTP，業務規則在 use case
type RoleHandler struct {
	roles  roleUseCase
	logger *slog.Logger
}

// constructor
func NewRoleHandler(roles roleUseCase) *RoleHandler {
	return &RoleHandler{
		roles:  roles,
		logger: slog.Default().With("component", "room_roles"),
	}
}

// 把房間事件發到 Redis；WS 訊息格式在 handler，所以實作也放這裡，use case 只認得 interface
type RoomNotifier struct {
	pub    messagePublisher
	logger *slog.Logger
}

// constructor
func NewRoomNotifier(pub messagePublisher) *RoomNotifier {
	return &RoomNotifier{
		pub:    pub,
		logger: slog.Default().With("component", "room_roles"),
	}
}

// 同 NewRoomNotifier，但 log 的 component 是 messages：訊息廣播失敗的 log 維持原本 messages handler 的 component
func NewMessageNotifier(pub messagePublisher) *RoomNotifier {
	return &RoomNotifier{
		pub:    pub,
		logger: slog.Default().With("component", "messages"),
	}
}

// 廣播新訊息（WS 的 message envelope）
// CreateMessage 維持 request ctx：INSERT 之前 / 之中被取消 → 沒存也沒廣播，結果一致
// 廣播不跟著 request 取消：DB 已經 commit（副作用已發生），廣播是在完成這件事，
// 不該因為發起的人走了就停，否則房間內的觀眾要到重連才看得到這則
// WithoutCancel 會連 deadline 一起拿掉 → publishEnvelope 自己加 publishTimeout，Redis 卡住時不會無限等
// （client 在等 201，最多多等 500ms）；WithoutCancel 保留 ctx 的 value（request_id 等），log 仍串得起來
func (this *RoomNotifier) NotifyMessage(ctx context.Context, roomPublicID string, msg model.Message) {
	publishEnvelope(ctx, this.logger, this.pub, roomPublicID, wsEnvelope{Type: wsTypeMessage, Payload: msg})
}

// 廣播 user_hidden；失敗只記 log（publishEnvelope），不重試，也不影響已 commit 的角色變更
func (this *RoomNotifier) NotifyUserHidden(ctx context.Context, roomPublicID string, userID int64) {
	publishEnvelope(ctx, this.logger, this.pub, roomPublicID, userHiddenEnvelope(userID))
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

// PUT /rooms/{public_id}/roles/{user_id}：指派房管或 ban（冪等，成功回 204）
// flow：取 user id → 解析 path 的 user_id → 解析 body → 檢查 role → 交給 use case
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

	// API 合約：PUT 只收這兩個值，移除角色要用 DELETE（use case 本身也接受 viewer，所以要在這裡擋）
	role := model.RoomRole(req.Role)
	if role != model.RoomRoleModerator && role != model.RoomRoleBanned {
		this.writeRoleError(w, r, model.ErrInvalidRole, "change room role failed")
		return
	}

	err := this.roles.ChangeRole(r.Context(), r.PathValue("public_id"), actorID, targetID, role)
	if err != nil {
		this.writeRoleError(w, r, err, "change room role failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	err := this.roles.ChangeRole(r.Context(), r.PathValue("public_id"), actorID, targetID, model.RoomRoleViewer)
	if err != nil {
		this.writeRoleError(w, r, err, "change room role failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// 把 use case 回的 error 翻成 HTTP 回應；不認得的一律記 log 回 500
// code 是 API 合約，不能改名
func (this *RoleHandler) writeRoleError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	switch {
	case errors.Is(err, model.ErrRoomNotFound):
		httpx.WriteError(w, http.StatusNotFound, "room_not_found", "room not found")
	case errors.Is(err, model.ErrUserNotFound):
		httpx.WriteError(w, http.StatusNotFound, "user_not_found", "user not found")
	case errors.Is(err, model.ErrRoomForbidden):
		httpx.WriteError(w, http.StatusForbidden, "room_forbidden", "forbidden")
	case errors.Is(err, model.ErrRoomTargetForbidden):
		httpx.WriteError(w, http.StatusForbidden, "room_target_forbidden", "cannot manage this user")
	case errors.Is(err, model.ErrRoomRoleConflict):
		httpx.WriteError(w, http.StatusConflict, "room_role_conflict", "unban the user before assigning a role")
	case errors.Is(err, model.ErrInvalidRole):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_role", "invalid role")
	default:
		this.logger.ErrorContext(r.Context(), logMsg, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

// GET /rooms/{public_id}/me：我在這間房的角色（owner / moderator / viewer）
// 回應看是誰在問，所以 no-store（放最前面，錯誤回應也不該被快取）
func (this *RoleHandler) HandleMyRole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := requireUserID(w, r, this.logger)
	if !ok {
		return
	}
	role, err := this.roles.MyRole(r.Context(), r.PathValue("public_id"), userID)
	if err != nil {
		this.writeRoleError(w, r, err, "get room role failed")
		return
	}
	httpx.WriteJSON(w, r, this.logger, http.StatusOK, myRoleResponse{Role: string(role)})
}

// GET /rooms/{public_id}/roles：角色名單，只有房主和房管看得到
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

	entries, next, err := this.roles.ListRoles(r.Context(), r.PathValue("public_id"), userID, after, limit)
	if err != nil {
		this.writeRoleError(w, r, err, "list room roles failed")
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
