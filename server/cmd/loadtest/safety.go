package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// 壓測工具的安全檢查：全部在做任何事（連線、寫入）之前跑，不合格就拒絕。
// 錯誤訊息只帶 host / database 名稱，絕不帶 URL 全文（裡面可能有密碼）。

const (
	// 壓測只准寫這個 DB 與這個 Redis db：與測試共用，跟 .githooks/pre-push 一致
	requiredDatabase = "danmaku_test"
	requiredRedisDB  = 15
)

// 本機的 host：只接受這兩個字串（不做 DNS 解析、不接受 ::1 或別的別名，寧可多擋）
func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1"
}

// deploy/fault/docker-compose.yml 的 service 名稱；只有 -in-compose 時才允許。
// 名單寫死在程式碼、不能從命令列加，避免把 prod 網址加進來
// nginx 在 M10b 才加進 compose（fault 情境：負載平衡器後面）；先保留在名單裡
var composeHosts = []string{"server-1", "server-2", "nginx", "postgres", "redis"}

// 在 isLocalHost 之外，-in-compose 開啟時也接受 composeHosts 裡的名稱（完全比對）
func isAllowedHost(host string, inCompose bool) bool {
	if isLocalHost(host) {
		return true
	}
	return inCompose && slices.Contains(composeHosts, host)
}

// 拒絕訊息裡的「允許清單」說明：沒開 -in-compose 維持原樣（只有本機），開了就列出 composeHosts
func allowedHostsText(inCompose bool) string {
	if inCompose {
		return "only localhost, 127.0.0.1 and the compose hosts (" + strings.Join(composeHosts, ", ") + ") are allowed"
	}
	return "only localhost and 127.0.0.1 are allowed"
}

// 檢查要壓的 server URL：http(s)、host 必須是 localhost / 127.0.0.1、不可帶帳密，path 只能是空或 /
// 回傳去掉結尾 / 的 base URL
func checkServerURL(raw string, inCompose bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("server URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL %q: scheme must be http or https", safeURLLabel(u))
	}
	if u.User != nil {
		return "", errors.New("server URL must not contain credentials")
	}
	if !isAllowedHost(u.Hostname(), inCompose) {
		return "", fmt.Errorf("server URL host %q is not allowed: %s", u.Hostname(), allowedHostsText(inCompose))
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server URL %q must be just scheme://host:port", safeURLLabel(u))
	}
	return u.Scheme + "://" + u.Host, nil
}

// 一次檢查全部 server URL；至少要有一個。回傳 base URL 列表
func checkServerURLs(raws []string, inCompose bool) ([]string, error) {
	var bases []string
	var errs []error
	for _, raw := range raws {
		base, err := checkServerURL(raw, inCompose)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		bases = append(bases, base)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		return nil, errors.New("no server URL given (-servers)")
	}
	return bases, nil
}

// 給錯誤訊息用的 URL 標籤：不含帳密
func safeURLLabel(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

// 檢查 TEST_DATABASE_URL：每個 host（含備援 host）都要是本機，database 必須剛好是 danmaku_test
// 回傳解析出的密碼，給 scrubber 遮蔽用
func checkDatabaseURL(raw string, inCompose bool) (password string, err error) {
	if raw == "" {
		return "", errors.New("TEST_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		// 不把 pgx 的錯誤往外傳：保險起見，不讓連線字串的任何片段出現在訊息裡
		return "", errors.New("TEST_DATABASE_URL: cannot parse connection string")
	}
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	for _, h := range hosts {
		if !isAllowedHost(h, inCompose) {
			return "", fmt.Errorf("TEST_DATABASE_URL host %q is not allowed: %s", h, allowedHostsText(inCompose))
		}
	}
	if cfg.Database != requiredDatabase {
		return "", fmt.Errorf("TEST_DATABASE_URL database is %q, must be exactly %q", cfg.Database, requiredDatabase)
	}
	return cfg.Password, nil
}

// 檢查 TEST_REDIS_URL：host 必須是本機、db 必須是 15。回傳密碼給 scrubber 用
func checkRedisURL(raw string, inCompose bool) (password string, err error) {
	if raw == "" {
		return "", errors.New("TEST_REDIS_URL is not set")
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		return "", errors.New("TEST_REDIS_URL: cannot parse URL")
	}
	host, _, splitErr := net.SplitHostPort(opt.Addr)
	if splitErr != nil || !isAllowedHost(host, inCompose) {
		return "", fmt.Errorf("TEST_REDIS_URL host %q is not allowed: %s", host, allowedHostsText(inCompose))
	}
	if opt.DB != requiredRedisDB {
		return "", fmt.Errorf("TEST_REDIS_URL db is %d, must be exactly %d", opt.DB, requiredRedisDB)
	}
	return opt.Password, nil
}

// 把 secret（JWT secret、DB / Redis 密碼）從要印出去的字串裡遮掉。太短的 secret 不遮（會誤傷一般文字）
type scrubber struct {
	secrets []string
}

func (s *scrubber) add(secret string) {
	if len(secret) >= 4 {
		s.secrets = append(s.secrets, secret)
	}
}

func (s *scrubber) scrub(text string) string {
	for _, secret := range s.secrets {
		text = strings.ReplaceAll(text, secret, "***")
	}
	return text
}

// 錯誤轉成可以安全印出的字串
func (s *scrubber) err(err error) string {
	if err == nil {
		return ""
	}
	return s.scrub(err.Error())
}
