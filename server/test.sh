#!/bin/sh
# 載入 .env.test 後跑 go test（-race -p 1）。
# 用法：
#   ./test.sh                                          # 跑 ./...
#   ./test.sh ./internal/pgstore -run X -count=1       # 自訂 package / 參數
# -p 1：store 測試共用同一個 DB，不能並行。
set -eu

cd "$(dirname "$0")"

if [ ! -f .env.test ]; then
	echo "找不到 server/.env.test，請先在 server/ 執行：cp .env.test.example .env.test" >&2
	exit 1
fi

set -a
. ./.env.test
set +a

[ $# -eq 0 ] && set -- ./...
exec go test -race -p 1 "$@"
