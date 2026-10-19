package store

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jay41chen/danmaku/server/migrations"
)

func RunMigration(dbURL string) error {
	// 用 pgx 的 parser（跟 NewDBStore 的 pgxpool 同一個，照 libpq 的連線字串規格），不自己解析 URL：
	// 1. 同一串 DATABASE_URL，store 能用、migration 就能用（例如密碼裡沒 encode 的 #，pgx 也吃得下）
	// 2. 支援 DSN 格式（host=... user=...）
	// 3. 解析失敗時，pgx 會盡量（best-effort）把錯誤訊息裡的密碼遮成 xxxxx；連線失敗的錯誤只帶 user / database
	//    前提是密碼的特殊字元已經 percent-encode：沒 encode 的 @ 會讓 URL 被切錯，密碼片段可能被當成 host / database 印進 log
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("parse db url: %w", err)
	}

	// migrate 由兩種 driver 組成：
	//
	//      [SQL 檔從哪來] → migrate → [寫進哪個 DB]
	//       source driver              database driver
	//       iofs ← 這裡                pgxmigrate.WithInstance（下面）
	//
	// iofs 的名字來自標準庫的 io/fs package，不是 stream：
	// 它能讀任何實作 fs.FS 介面的東西，embed.FS 就是其中一種
	// 第二個參數是 FS 裡的路徑："." 代表 FS 根目錄（embed 寫的是 *.sql，檔案直接在根目錄）
	//
	// 不用 file driver（file://migrations）：它讀的是執行時硬碟上的資料夾，
	// 部署時要另外帶檔案；embed 在 compile 時就放進 binary，只要帶執行檔
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("load migration files: %w", err)
	}

	// migration 自己開一條連線，跟 store 的 pgxpool 分開；成功建立 migrator 之後，m.Close() 會一起關掉
	db := stdlib.OpenDB(*cfg)
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		db.Close()
		src.Close()
		return fmt.Errorf("connect db: %w", err)
	}

	// 建立migrator
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		driver.Close()
		src.Close()
		return fmt.Errorf("create migrator: %w", err)
	}
	// 記得defer，不然會有leak
	defer m.Close()
	// migrate.ErrNoChange 代表不用更新，所以要exclude掉這個error
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
