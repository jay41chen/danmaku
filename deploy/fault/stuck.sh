#!/bin/sh
# 在 fault 環境的 Docker 網路裡跑 `loadtest -scenario stuck`，參數直接轉給工具（例如 -stuck 3000 -stuck-rate 50 -duration 180s）。
# 為什麼要在網路裡：stuck client 必須跟 server 同一個網路，否則 server 面對的是 Docker Desktop 的 port-forward proxy，不是 client。
# 環境變數：OUT_DIR（輸出目錄，預設 deploy/fault/out/）、OUT_NAME（檔名，預設 stuck-<時間>.json）
# 注意：stuck 的 -duration 預設 90s，必須大於 server 的 75s pong timeout，否則踢不到
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

out_dir="${OUT_DIR:-$here/out}"
out_name="${OUT_NAME:-stuck-$(date +%Y%m%d-%H%M%S).json}"
mkdir -p "$here/.bin" "$out_dir"

(cd "$here/../../server" && GOOS=linux CGO_ENABLED=0 go build -o "$here/.bin/loadtest" ./cmd/loadtest)

# 實際被打的 server：取 "$@" 裡最後一個 -servers（工具也是最後一個生效），沒給就是 server-1
servers_arg="http://server-1:8080"
prev=""
for a in "$@"; do
  case "$prev" in -servers|--servers) servers_arg="$a" ;; esac
  case "$a" in -servers=*|--servers=*) servers_arg="${a#*=}" ;; esac
  prev="$a"
done
# URL 轉成 compose service 名稱（http://server-2:8080 → server-2）
targets=""
for u in $(printf '%s' "$servers_arg" | tr ',' ' '); do
  h="${u#*://}"; h="${h%%/*}"; h="${h%%:*}"
  targets="$targets $h"
done

start_ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
rc=0
# 容器內用目前使用者的 uid:gid 寫輸出（distroless 預設是 nonroot 65532，寫不進掛載目錄）
docker run --rm --user "$(id -u):$(id -g)" --network danmaku-fault_default --env-file "$here/.env" \
  -e TEST_DATABASE_URL='postgres://fault:fault@postgres:5432/danmaku_test?sslmode=disable' \
  -e TEST_REDIS_URL='redis://redis:6379/15' \
  -v "$here/.bin:/bin-loadtest:ro" -v "$out_dir:/out" \
  gcr.io/distroless/static-debian12:nonroot \
  /bin-loadtest/loadtest -in-compose -scenario stuck -servers "$servers_arg" \
  -require-machine-check=false -docker-containers "" -out "/out/$out_name" "$@" || rc=$?

# 一個連線被踢可能印不只一行，所以這是 log 行數，不是被踢的連線數；工具失敗時也照印
for s in $targets; do
  if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" "$s" 2>/dev/null)"; then
    n="$(printf '%s\n' "$logs" | grep -c "ws outbox full" || true)"
  else
    n="n/a (logs failed)"
  fi
  printf 'ws outbox full kicks, log 行數 / log lines (%s, since %s): %s\n' "$s" "$start_ts" "$n"
done
exit "$rc"
