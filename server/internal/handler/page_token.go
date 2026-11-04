package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	// bytes、encoding/base64、encoding/json
	"github.com/jay41chen/danmaku/server/internal/model"
)

// page token：GET /rooms、GET /me/rooms 回給 client 的 next_page_token，client 下一頁原樣帶回來
// 格式：JSON {"v":1,"created_at":"<RFC3339Nano>","public_id":"..."} → base64.RawURLEncoding
// 對 client 是 opaque：只能原樣帶回、不該自己解析或組裝
// 這是約定，不是安全機制（沒加密、沒簽章，誰都能 decode 來看）；所以 decode 時要把它當成任意的使用者輸入

// 格式改版時 +1，舊版的 token 就會被拒絕（client 從第一頁重來）
const pageTokenVersion = 1

// 任何一項不合格都回這個，不告訴 client 是哪一項壞掉
var errInvalidPageToken = errors.New("invalid cursor")

// token 裡的 JSON；只在 handler 內部用，不對外
type pageToken struct {
	V         int       `json:"v"`
	CreatedAt time.Time `json:"created_at"` // time.Time 的 JSON 預設就是 RFC3339Nano，微秒不會丟
	PublicID  string    `json:"public_id"`
}

// 回傳 error 的情況幾乎不會發生：只有 created_at 的年份超出 0–9999 時，time.Time 的 MarshalJSON 才會失敗
// err 原樣往上回（跟 decode 不一樣）：這是 server 自己的問題，handler 記 log + 回 500，要留著原因查問題
func encodePageToken(c model.RoomCursor) (string, error) {
	token := pageToken{
		CreatedAt: c.CreatedAt,
		PublicID:  c.PublicID,
		V:         pageTokenVersion,
	}
	b, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString(b)
	return encode, nil
}

// parse, don't validate：所有檢查都在這裡做完，
// 回傳的 RoomCursor 可以直接拿去查 DB
func decodePageToken(s string) (model.RoomCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return model.RoomCursor{}, errInvalidPageToken
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	// 多一個不認識的欄位就拒絕
	decoder.DisallowUnknownFields()
	var token pageToken
	// 一定要檢查 err：Decoder 是邊讀邊寫進 token 的，遇到不認識的欄位才回錯；
	// 不檢查的話前面的欄位已經填好，DisallowUnknownFields 等於沒作用
	if err := decoder.Decode(&token); err != nil {
		return model.RoomCursor{}, errInvalidPageToken
	}
	// Decode 只讀第一個 JSON 值就停，後面接的東西（{}、x、第二個物件）不會報錯 → 要自己確認已經讀到結尾
	// 再讀一次要剛好是 io.EOF；讀到任何東西（包括語法錯誤）都拒絕
	// 不用 decoder.More()：它看到 } 或 ] 會回 false，`{...}}` 會漏掉
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return model.RoomCursor{}, errInvalidPageToken
	}
	// V 不對（舊版 / 改過的 token）、時間是零值（欄位沒給）、public_id 格式錯 → 都不能拿去查 DB
	if token.V != pageTokenVersion || token.CreatedAt.IsZero() || !model.ValidPublicID(token.PublicID) {
		return model.RoomCursor{}, errInvalidPageToken
	}

	return model.RoomCursor{
		CreatedAt: token.CreatedAt,
		PublicID:  token.PublicID,
	}, nil
}
