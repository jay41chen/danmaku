package model

import (
	"time"
)

// UserID / Username 用指標：nil 序列化成 JSON null
// null = 舊資料（沒有發送者）或發送者已被刪除；client 看到 null 就顯示 placeholder
// （「已刪除的用戶」）。不由 server 填一個字串進去：client 才能做 i18n，
// 也避免有人註冊一個跟 placeholder 同名的帳號來冒名
type Message struct {
	ID        int64     `json:"id"`
	UserID    *int64    `json:"user_id"`
	Username  *string   `json:"username"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
