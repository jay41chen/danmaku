package auth

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/jay41chen/danmaku/server/internal/randx"
)

func HashRefreshToken(plain string) string {
	hash := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(hash[:])
}

// plain：回給 client 的原文；hash：存進 DB 的
func NewRefreshToken() (plain string, hash string) {
	plain = randx.URLText(32)
	hash = HashRefreshToken(plain)
	return plain, hash
}
