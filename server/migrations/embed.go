package migrations

import (
	"embed"
)

// 下方那一行`//go:embed *.sql`不是註解，
// 而是寫給compiler看的annotation
// compile 時把 SQL 檔放進 binary，類似 Kotlin 打包進 jar 的 resources
// 特別注意：`//go:embed` 只看得到自己所在資料夾和底下的檔案

//go:embed *.sql
var FS embed.FS
