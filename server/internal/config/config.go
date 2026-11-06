// 啟動設定：集中讀 env、套預設值、檢查格式。設定錯誤一次全部回報
package config

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/middleware"
)

// 沒設時的預設值：都是本機開發用的（docker-compose 對外的 port）
const (
	defaultPort        = "8080"
	defaultDatabaseURL = "postgres://danmaku:danmaku@localhost:3192/danmaku"
	defaultRedisURL    = "redis://localhost:2907"

	// 預設值 = 沒有壓測時 production 的行為；WS_MAX_CLIENTS 只給壓測調整用
	DefaultWSMaxClients = 100
	// 上限只是擋誤設
	maxPositiveSetting = 1_000_000
)

type Config struct {
	Port           string
	DatabaseURL    string
	RedisURL       string
	CORSOrigins    []string // 已經用 middleware.ParseOrigins 解析過
	ClientIPHeader string   // 空字串 = 用 RemoteAddr
	JWTSecret      string

	WSMaxClients      int  // 這台 server 的 WS 連線上限（Hub）
	LoadtestTimingLog bool // 壓測用：每 5 秒 log 一行 DB 計時（見 internal/loadtiming），預設關
}

// 讀一個「正整數」env：沒設用預設值；不是整數、<= 0、超過上限都回 error（錯誤訊息帶 env 名稱，但不帶使用者輸入以外的東西）
func positiveIntEnv(getenv func(string) string, name string, def int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxPositiveSetting {
		return 0, fmt.Errorf("%s: want an integer between 1 and %d, got %q", name, maxPositiveSetting, raw)
	}
	return n, nil
}

// 讀一個 bool env（1 / true / 0 / false 等 strconv.ParseBool 接受的寫法）：沒設 = false，寫錯就 error，不默默當成 false
func boolEnv(getenv func(string) string, name string) (bool, error) {
	raw := getenv(name)
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: want a boolean (1, true, 0, false), got %q", name, raw)
	}
	return b, nil
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

	wsMaxClients, err := positiveIntEnv(getenv, "WS_MAX_CLIENTS", DefaultWSMaxClients)
	if err != nil {
		errs = append(errs, err)
	}
	timingLog, err := boolEnv(getenv, "LOADTEST_TIMING_LOG")
	if err != nil {
		errs = append(errs, err)
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

		WSMaxClients:      wsMaxClients,
		LoadtestTimingLog: timingLog,
	}, nil
}
