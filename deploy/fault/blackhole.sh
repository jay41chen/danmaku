#!/bin/sh
# 跑 `loadtest -scenario redis`，中途在 server-1 的網路 namespace 裡用 iptables DROP 掉它與 Redis 之間的封包（黑洞：不回 RST，連線只會卡住 / timeout），一段時間後再移除。
# 用法：blackhole.sh <blackhole_seconds> [額外的 loadtest 參數，例如 -duration 180s -redis-viewers 10]
# 設計重點：
#   - 觀眾直連 server-1 / server-2（不經 nginx），所以知道每條連線落在哪一台、哪一台是故障的那台。
#   - 規則只影響 server-1 <-> redis；Redis 本身沒停，server-2 對 Redis 的連線完全正常（對照組）。
# 流程：背景啟動 loadtest（跑在 danmaku-fault 網路裡）→ 等 30s → 記 T_down → 加 DROP 規則 → 等 N 秒 → 移除規則 → 記 T_up → 等 loadtest 結束。
# 規則是用一個用完即丟的 nicolaka/netshoot 容器（--network container:<server-1>，加 NET_ADMIN）加在 server-1 的 namespace 裡，不動 server-1 容器本身。
# 環境變數：OUT_DIR（輸出目錄，預設 deploy/fault/out/）、OUT_NAME（檔名，預設 blackhole-<秒數>-<時間>.json）
# 注意：loadtest 的 -duration 預設 120s，必須大於 30 + N + 恢復後想觀察的時間
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <blackhole_seconds> [extra loadtest flags]" >&2
  exit 2
fi
secs="$1"
shift 1
case "$secs" in ''|*[!0-9]*) echo "blackhole.sh: blackhole_seconds must be a non-negative integer, got '$secs'" >&2; exit 2 ;; esac

out_dir="${OUT_DIR:-$here/out}"
out_name="${OUT_NAME:-blackhole-$secs-$(date +%Y%m%d-%H%M%S).json}"
mkdir -p "$here/.bin" "$out_dir"

(cd "$here/../../server" && GOOS=linux CGO_ENABLED=0 go build -o "$here/.bin/loadtest" ./cmd/loadtest)

servers_arg="http://server-1:8080,http://server-2:8080"
targets="server-1 server-2"
netshoot="nicolaka/netshoot:v0.13"

# 不論怎麼結束（正常、失敗、Ctrl-C）都把規則移除；只處理 danmaku-fault 的 server-1
added=0
lt_pid=""
s1_id=""
redis_ip=""
# 在 server-1 的 namespace 裡執行一段 sh 指令（用完即丟的 netshoot 容器）
in_server1() {
  docker run --rm --network "container:$s1_id" --cap-add NET_ADMIN "$netshoot" sh -c "$1"
}
# 兩條規則各自移除（一條失敗不跳過另一條）；任一條失敗回 1 並印 WARNING
remove_rules() {
  failed=0
  in_server1 "iptables -D OUTPUT -d $redis_ip -j DROP" >/dev/null 2>&1 || failed=1
  in_server1 "iptables -D INPUT -s $redis_ip -j DROP" >/dev/null 2>&1 || failed=1
  if [ "$failed" = "1" ]; then
    echo "blackhole.sh: WARNING: failed to remove iptables rules on server-1 (redis $redis_ip); remove them manually" >&2
    return 1
  fi
  return 0
}
restore() {
  if [ "$added" = "1" ]; then
    remove_rules || true
    added=0
  fi
}
on_exit() {
  rc=$?
  trap - EXIT INT TERM
  if [ -n "$lt_pid" ] && kill -0 "$lt_pid" 2>/dev/null; then kill "$lt_pid" 2>/dev/null || true; fi
  restore
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# 先確保 netshoot image 在本機，否則第一次 docker run 的 pull 時間會算進 T_down 與 T_down 之後的觀察
docker image inspect "$netshoot" >/dev/null 2>&1 || docker pull "$netshoot"

start_ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# 容器內用目前使用者的 uid:gid 寫輸出（distroless 預設是 nonroot 65532，寫不進掛載目錄）
docker run --rm --user "$(id -u):$(id -g)" --network danmaku-fault_default --env-file "$here/.env" \
  -e TEST_DATABASE_URL='postgres://fault:fault@postgres:5432/danmaku_test?sslmode=disable' \
  -e TEST_REDIS_URL='redis://redis:6379/15' \
  -v "$here/.bin:/bin-loadtest:ro" -v "$out_dir:/out" \
  gcr.io/distroless/static-debian12:nonroot \
  /bin-loadtest/loadtest -in-compose -scenario redis -servers "$servers_arg" \
  -require-machine-check=false -docker-containers "" -out "/out/$out_name" "$@" &
lt_pid=$!

sleep 30
t_down="n/a"
t_up="n/a"
if kill -0 "$lt_pid" 2>/dev/null; then
  s1_id="$(docker compose -p danmaku-fault ps -q server-1)"
  redis_id="$(docker compose -p danmaku-fault ps -q redis)"
  if [ -z "$s1_id" ] || [ -z "$redis_id" ]; then
    echo "blackhole.sh: cannot find the server-1 or redis container in project danmaku-fault" >&2
    exit 1
  fi
  redis_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "danmaku-fault_default"}}{{.IPAddress}}{{end}}' "$redis_id")"
  if [ -z "$redis_ip" ]; then
    echo "blackhole.sh: cannot resolve redis IP on network danmaku-fault_default" >&2
    exit 1
  fi
  echo "blackhole.sh: redis IP $redis_ip; dropping server-1 <-> redis for ${secs}s"
  added=1 # 先標記再加：加到一半失敗也會在 restore 嘗試移除（-D 失敗只會印 WARNING）
  in_server1 "iptables -I OUTPUT -d $redis_ip -j DROP && iptables -I INPUT -s $redis_ip -j DROP"
  if ! in_server1 "iptables -C OUTPUT -d $redis_ip -j DROP && iptables -C INPUT -s $redis_ip -j DROP"; then
    echo "blackhole.sh: iptables rules not present on server-1 after adding (redis $redis_ip)" >&2
    exit 1
  fi
  t_down="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  sleep "$secs"
  if remove_rules; then added=0; fi # 失敗時保留 added=1，trap 會再試一次
  t_up="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
else
  echo "blackhole.sh: loadtest exited before the blackhole, skipping" >&2
fi

rc=0
wait "$lt_pid" || rc=$?
lt_pid=""

printf 'run start: %s\nT_down (blackhole server-1 <-> redis %s): %s\nT_up: %s\nloadtest exit code: %s\n' "$start_ts" "${redis_ip:-n/a}" "$t_down" "$t_up" "$rc"

# 各種 log 行數（不是事件數：一個事件可能印多行）；logs 失敗就印 n/a；loadtest 失敗時也照印
for s in $targets; do
  if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" "$s" 2>/dev/null)"; then
    for pat in "publish to redis failed" "rate limiter failed" "pubsub judged deaf, rebuilding" "publish heartbeat failed" "subscribe room failed, will retry" "create message failed" "presence sync failed" "presence count failed" "unsubscribe room failed"; do
      n="$(printf '%s\n' "$logs" | grep -c "$pat" || true)"
      printf 'log lines "%s" (%s, since %s): %s\n' "$pat" "$s" "$start_ts" "$n"
    done
  else
    printf 'log lines (%s, since %s): n/a (logs failed)\n' "$s" "$start_ts"
  fi
done
exit "$rc"
