// 啟動設定：集中讀 env、套預設值、檢查格式。設定錯誤一次全部回報
package config

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/middleware"
)

// 沒設時的預設值：都是本機開發用的（docker-compose 對外的 port）
const (
	defaultPort        = "8080"
	defaultDatabaseURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	defaultRedisURL    = "redis://localhost:2907"
)

type Config struct {
	Port           string
	DatabaseURL    string
	RedisURL       string
	CORSOrigins    []string // 已經用 middleware.ParseOrigins 解析過
	ClientIPHeader string   // 空字串 = 用 RemoteAddr
	JWTSecret      string
}

// getenv 由呼叫端傳入：main 傳 os.Getenv，test 傳假的，config 本身不碰全域狀態
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	// cmp.Or 的規則
	// 從左到右，回傳第一個「不是空的」參數；如果全部都是空的，就回傳空字串。
	port := cmp.Or(getenv("PORT"), defaultPort)
	dbURL := cmp.Or(getenv("DATABASE_URL"), defaultDatabaseURL)
	redisURL := cmp.Or(getenv("REDIS_URL"), defaultRedisURL)
	clientIPHeader := cmp.Or(getenv("CLIENT_IP_HEADER"))
	// cors origin要走parser檢查一下
	corsOrigin := cmp.Or(getenv("CORS_ORIGINS"))
	validOrigins, err := middleware.ParseOrigins(corsOrigin)
	if err != nil {
		errs = append(errs, fmt.Errorf("CORS_ORIGINS: %w", err))
	}
	// jwt secret要檢查長度
	jwtSecret := cmp.Or(getenv("JWT_SECRET"))
	if err := auth.ValidateJWTSecretLength(jwtSecret); err != nil {
		errs = append(errs, fmt.Errorf("JWT_SECRET: %w", err))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return Config{
		Port:           port,
		DatabaseURL:    dbURL,
		RedisURL:       redisURL,
		ClientIPHeader: clientIPHeader,
		CORSOrigins:    validOrigins,
		JWTSecret:      jwtSecret,
	}, nil
}
