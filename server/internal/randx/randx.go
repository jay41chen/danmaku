// Package randx：跟 httpx 同樣的命名方式，放「隨機值」相關的小工具；不叫 common / util，Go 慣例是依用途命名
package randx

import (
	"crypto/rand"
	"encoding/base64"
)

// 產生 nBytes 個隨機 bytes 並編成 URL 安全的文字（refresh token、room public_id 共用）
func URLText(nBytes int) string {
	b := make([]byte, nBytes)
	// 要用 crypto/rand（OS 提供、無法預測），不能用 math/rand（給遊戲 / 模擬用，看過一部分輸出就可能推出後面的）
	// 不檢查 error 是安全的：Go 1.24 起 crypto/rand.Read 永遠不會回傳 error（失敗會直接讓程式 crash），go.mod 是 go 1.27.1
	rand.Read(b)
	// 原始 bytes 會有 NUL、換行、"、非法 UTF-8，不能直接放進 JSON / header → 轉成安全字元
	// RawURLEncoding = URL 安全字母表（- _ 取代 + /）+ 不補結尾的 =（Raw），跟 JWT 用的同一種
	// 編碼後長度約為 nBytes × 4/3（無 padding，向上取整），例如 32 bytes → 43 字元、8 bytes → 11 字元
	return base64.RawURLEncoding.EncodeToString(b)
}
