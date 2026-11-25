package pgstore

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jay41chen/danmaku/server/migrations"
)

func RunMigration(dbURL string) error {
	// 用 pgxpool 的 parser（跟 NewDBStore 完全同一個，底層是 pgx 照 libpq 的連線字串規格解析），不自己解析 URL：
	// 1. 同一串 DATABASE_URL，store 能用、migration 就能用（例如密碼裡沒 encode 的 #，pgx 也吃得下；
	//    pool_* 參數如 pool_max_conns 也一樣：pgxpool 會從 RuntimeParams 拿掉，不會被當成 runtime param 轉給 Postgres，
	//    不然 Postgres 會回 FATAL: unrecognized configuration parameter）
	// 2. 支援 DSN 格式（host=... user=...）
	// 3. 解析失敗時，pgx 會盡量（best-effort）把錯誤訊息裡的密碼遮成 xxxxx；連線失敗的錯誤只帶 user / database
	//    前提是密碼的特殊字元已經 percent-encode：沒 encode 的 @ 會讓 URL 被切錯，密碼片段可能被當成 host / database 印進 log
	poolCfg, err := pgxpool.ParseConfig(dbURL)
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

	// migration 自己開一條連線（只取 ConnConfig，pool 本身不用），跟 store 的 pgxpool 分開；成功建立 migrator 之後，m.Close() 會一起關掉
	db := stdlib.OpenDB(*poolCfg.ConnConfig)
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
	applied := true
	if err := m.Up(); err != nil {
		if !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("apply migrations: %w", err)
		}
		applied = false
	}

	// 以前成功時沒有任何 log，要知道 schema 版本得連進 DB 查；這裡記一行：版本、有沒有 dirty、這次有沒有真的套用
	version, dirty, err := m.Version()
	if err != nil {
		// 還沒套用過任何 migration 時回 ErrNilVersion，當成版本 0
		if !errors.Is(err, migrate.ErrNilVersion) {
			return fmt.Errorf("read migration version: %w", err)
		}
		version, dirty = 0, false
	}
	slog.Default().With("component", "db").Info("migration done", "version", version, "dirty", dirty, "applied", applied)
	return nil
}
