package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

func HashRefreshToken(plain string) string {
	hash := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(hash[:])
}

// plain：回給 client 的原文；hash：存進 DB 的
func NewRefreshToken() (plain string, hash string) {
	plain = Generate32ByteRandonText()
	hash = HashRefreshToken(plain)
	return plain, hash
}

func Generate32ByteRandonText() string {
	b := make([]byte, 32)
	// 要用 crypto/rand（OS 提供、無法預測），不能用 math/rand（給遊戲 / 模擬用，看過一部分輸出就可能推出後面的）
	rand.Read(b)
	// 原始 bytes 會有 NUL、換行、"、非法 UTF-8，不能直接放進 JSON / header → 轉成安全字元
	// RawURLEncoding = URL 安全字母表（- _ 取代 + /）+ 不補結尾的 =（Raw），跟 JWT 用的同一種
	// 32 bytes 編碼後是 43 字元（長度變 4/3 倍）
	return base64.RawURLEncoding.EncodeToString(b)
}
