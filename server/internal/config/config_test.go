package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/auth"
)

var validSecret = strings.Repeat("k", auth.MinSecretLength)

func TestLoad(t *testing.T) {
	t.Run("unset optional values use defaults", func(t *testing.T) {
		cfg, err := Load(fakeEnv(map[string]string{"JWT_SECRET": validSecret}))

		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		want := Config{
			Port:           defaultPort,
			DatabaseURL:    defaultDatabaseURL,
			RedisURL:       defaultRedisURL,
			CORSOrigins:    []string{"*"},
			ClientIPHeader: "",
			JWTSecret:      validSecret,
		}
		assertConfig(t, cfg, want)
	})

	t.Run("set values are used", func(t *testing.T) {
		cfg, err := Load(fakeEnv(map[string]string{
			"PORT":             "9000",
			"DATABASE_URL":     "postgres://u:p@db:5432/x",
			"REDIS_URL":        "redis://cache:6379",
			"CORS_ORIGINS":     "https://a.com,http://localhost:5173",
			"CLIENT_IP_HEADER": "X-Real-IP",
			"JWT_SECRET":       validSecret,
		}))

		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		want := Config{
			Port:           "9000",
			DatabaseURL:    "postgres://u:p@db:5432/x",
			RedisURL:       "redis://cache:6379",
			CORSOrigins:    []string{"https://a.com", "http://localhost:5173"},
			ClientIPHeader: "X-Real-IP",
			JWTSecret:      validSecret,
		}
		assertConfig(t, cfg, want)
	})
}

// 每個錯誤訊息都要帶 env 名稱：看 log 的人才知道要去改哪一個
func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantInErr []string
	}{
		{"jwt secret missing", map[string]string{}, []string{"JWT_SECRET"}},
		{"jwt secret too short", map[string]string{"JWT_SECRET": "short-s3kr1t"}, []string{"JWT_SECRET"}},
		{"invalid cors origins", map[string]string{"JWT_SECRET": validSecret, "CORS_ORIGINS": "https://a.com/path"}, []string{"CORS_ORIGINS"}},
		// 一次回報全部：不能遇到第一個錯就 return
		{"reports all errors at once", map[string]string{"CORS_ORIGINS": "https://a.com/path"}, []string{"JWT_SECRET", "CORS_ORIGINS"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(fakeEnv(tc.env))

			if err == nil {
				t.Fatalf("Load() error = nil, want error containing %q", tc.wantInErr)
			}
			for _, s := range tc.wantInErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error = %q, want it to contain %q", err, s)
				}
			}
		})
	}

	// 錯誤會被印進 log：只能說長度不夠，不能帶出 secret 本身
	t.Run("error does not leak jwt secret", func(t *testing.T) {
		_, err := Load(fakeEnv(map[string]string{"JWT_SECRET": "short-s3kr1t"}))

		if err == nil || strings.Contains(err.Error(), "s3kr1t") {
			t.Errorf("error = %v, want error without the secret", err)
		}
	})
}

// 假的 getenv：從 map 查，沒有的 key 回空字串（跟 os.Getenv 一樣）
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func assertConfig(t *testing.T, got, want Config) {
	t.Helper()
	// struct 裡只要有 slice 欄位就不能用 == 比（compile error，即使 slice 是 nil）：
	// slice 是「指向底層陣列 + 長度 + 容量」，Go 不替你決定 == 是比「同一塊記憶體」還是「內容一樣」
	// reflect.DeepEqual（標準庫）在執行時遞迴比內容：slice 逐個元素、map 逐個 key/value
	// 注意：
	//   - nil slice 跟空 slice（[]string{}）會被當成不一樣
	//   - 比較慢（執行時檢查型別），通常只在 test 用
	//   - 不相等時不會告訴你哪裡不一樣 → 錯誤訊息用 %+v 把兩邊都印出來
	// 只比一個 slice 的話用 slices.Equal 就好
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Config = %+v, want %+v", got, want)
	}
}
