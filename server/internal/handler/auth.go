package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/store"
)

const (
	msgInvalidCredentials = "invalid username or password"
)

var (
	dummyHash, _ = auth.HashPassword("timing-equalizer")
)

type AuthHandler struct {
	db     store.DBStoreInterface
	logger *slog.Logger
}

func NewAuthHandler(db store.DBStoreInterface) *AuthHandler {
	return &AuthHandler{
		db:     db,
		logger: slog.Default().With("component", "auth"),
	}
}

// register / login 共用的 request body
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// 回給 client 的 user：只放能公開的欄位（不含 hash）
type userResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// flow:
// 檢查username
// 檢查password
// 檢查hash
// 試著創造user
// 回傳結果
func (this *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodPost}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req credentials
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := auth.ValidateUsername(req.Username); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := auth.ValidatePassword(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// hash 後面還要用，所以不能寫成 if hash, err := ...; 那樣 hash 只活在 if 裡面
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		// 只記 err，不能記 req.Password
		this.logger.ErrorContext(r.Context(), "hash password failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	id, err := this.db.CreateUser(r.Context(), req.Username, hash)
	// 先比對已知的 sentinel，剩下的才是真的系統錯誤
	if errors.Is(err, store.ErrUsernameTaken) {
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "create user failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// header 要在 WriteHeader 之前設，之後設會被忽略
	setContentType(w)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(userResponse{ID: id, Username: req.Username})
}

func (this *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !checkMethodValidation(r, []string{http.MethodPost}) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req credentials
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// 查詢user在不在
	user, err := this.db.GetUser(r.Context(), req.Username)
	if errors.Is(err, store.ErrUserNotFound) {
		// 結果不重要，目的是花跟「密碼錯」一樣的時間
		// 借以避免讓攻擊者知道錯誤
		_, _ = auth.CheckPassword(dummyHash, req.Password)
		http.Error(w, msgInvalidCredentials, http.StatusUnauthorized)
		return
	}
	if err != nil {
		this.logger.ErrorContext(r.Context(), "get user failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 有user，檢查密碼是不是正確的
	success, err := auth.CheckPassword(user.PasswordHash, req.Password)
	if err != nil {
		this.logger.ErrorContext(r.Context(), "check password failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !success {
		http.Error(w, msgInvalidCredentials, http.StatusUnauthorized)
		return
	}

	setContentType(w)
	w.WriteHeader(http.StatusOK)
	// 理論上要發JWT，但還沒做到，先這樣
	// 用 DB 存的 username：用 jay 登入，註冊時存的是 Jay，前端要顯示 Jay
	json.NewEncoder(w).Encode(userResponse{ID: user.ID, Username: user.Username})
}
