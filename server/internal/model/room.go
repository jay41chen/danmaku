package model

import "time"

type Room struct {
	ID            int64     `json:"-"` // 內部 PK，不對外
	PublicID      string    `json:"public_id"`
	OwnerID       int64     `json:"-"`
	OwnerUsername string    `json:"owner_username"` // 網址 /{username}/{public_id} 用
	Name          string    `json:"room_name"`
	CreatedAt     time.Time `json:"created_at"`
}
