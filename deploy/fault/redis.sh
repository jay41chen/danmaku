#!/bin/sh
# 跑 `loadtest -scenario $SCENARIO`（預設 redis），中途把 fault 環境的 Redis stop 或 pause 一段時間，再恢復。
# 用法：redis.sh <stop|pause> <outage_seconds> [額外的 loadtest 參數，例如 -duration 180s -redis-viewers 10]
# 流程：背景啟動 loadtest（跑在 danmaku-fault 網路裡，stdout 同時寫進 log 檔並即時顯示、stderr 直接到終端機）→ 輪詢 log 等到 loadtest 印出「start Redis fault now via redis.sh」（觀眾都 join 完、開始量測）
#       → 再等 FAULT_OFFSET 秒 → 記 T_down → stop/pause redis → 等 outage 秒 → start/unpause → 記 T_up → 等 loadtest 結束。
# 這樣 fault 的時間點是相對「量測開始」，不受 seeding / 觀眾 join 花多久影響（-spike-rooms 很大時 join 可能超過 30s）。
# 注意：stop + start 會丟掉 Redis 資料（tmpfs、沒開持久化），這是刻意的；pause 則保留資料、連線只是被凍住。
# 例：SCENARIO=resync-spike redis.sh stop 10 -duration 90s   # 量 Redis 恢復時的歷史 GET 尖峰（見 server/cmd/loadtest/spike.go）
# 環境變數：SCENARIO（loadtest 情境，預設 redis；resync-spike 的預設 -duration 是 90s）、OUT_DIR（輸出目錄，預設 deploy/fault/out/）、OUT_NAME（檔名，預設 <scenario>-<mode>-<秒數>-<時間>.json）
# 環境變數（時序）：FAULT_OFFSET（看到 marker 後再等幾秒才動 Redis，預設 20）、MARKER_TIMEOUT（最多等 marker 幾秒，預設 600；逾時就 kill loadtest、印錯誤到 stderr、以 exit 1 結束，不做 fault）
# 注意：SCENARIO 必須是會印出 marker 的情境（目前是 redis 和 resync-spike），其他情境會一直等到 MARKER_TIMEOUT
# 注意：loadtest 的 -duration 預設 120s（resync-spike 是 90s），必須大於 FAULT_OFFSET + outage + 恢復後想觀察的時間（-duration 從量測開始算）
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <stop|pause> <outage_seconds> [extra loadtest flags]" >&2
  exit 2
fi
mode="$1"
outage="$2"
shift 2
case "$mode" in stop|pause) ;; *) echo "redis.sh: mode must be stop or pause, got '$mode'" >&2; exit 2 ;; esac
case "$outage" in ''|*[!0-9]*) echo "redis.sh: outage_seconds must be a non-negative integer, got '$outage'" >&2; exit 2 ;; esac

scenario="${SCENARIO:-redis}"
out_dir="${OUT_DIR:-$here/out}"
out_name="${OUT_NAME:-$scenario-$mode-$outage-$(date +%Y%m%d-%H%M%S).json}"
fault_offset="${FAULT_OFFSET:-20}"
marker_timeout="${MARKER_TIMEOUT:-600}"
case "$fault_offset" in ''|*[!0-9]*) echo "redis.sh: FAULT_OFFSET must be a non-negative integer, got '$fault_offset'" >&2; exit 2 ;; esac
case "$marker_timeout" in ''|*[!0-9]*) echo "redis.sh: MARKER_TIMEOUT must be a non-negative integer, got '$marker_timeout'" >&2; exit 2 ;; esac
marker="start Redis fault now via redis.sh"
mkdir -p "$here/.bin" "$out_dir"

(cd "$here/../../server" && GOOS=linux CGO_ENABLED=0 go build -o "$here/.bin/loadtest" ./cmd/loadtest)

servers_arg="http://server-1:8080,http://server-2:8080"
targets="server-1 server-2"

# 不論怎麼結束（正常、失敗、Ctrl-C）都把 Redis 救回來；只處理 danmaku-fault 的 redis
down=0
lt_pid=""
tail_pid=""
log="$out_dir/.${out_name%.json}.log"
stop_tail() {
  if [ -n "$tail_pid" ]; then
    kill "$tail_pid" 2>/dev/null || true
    { wait "$tail_pid"; } 2>/dev/null || true # 回收掉，免得 shell 印 "Terminated"
    tail_pid=""
  fi
}
restore() {
  if [ "$down" = "1" ]; then
    if [ "$mode" = "stop" ]; then
      docker compose -p danmaku-fault start redis >/dev/null 2>&1 || echo "redis.sh: WARNING: failed to start redis" >&2
    else
      docker compose -p danmaku-fault unpause redis >/dev/null 2>&1 || echo "redis.sh: WARNING: failed to unpause redis" >&2
    fi
    down=0
  fi
}
on_exit() {
  rc=$?
  trap - EXIT INT TERM
  # 還活著就 kill，並最多等 5 秒（每秒輪詢）讓 loadtest 把收尾輸出寫進 log；不無限等，免得卡住 Redis 的恢復
  if [ -n "$lt_pid" ] && kill -0 "$lt_pid" 2>/dev/null; then
    kill "$lt_pid" 2>/dev/null || true
    n=0
    while [ "$n" -lt 5 ] && kill -0 "$lt_pid" 2>/dev/null; do
      sleep 1
      n=$((n + 1))
    done
    if ! kill -0 "$lt_pid" 2>/dev/null; then { wait "$lt_pid"; } 2>/dev/null || true; fi
  fi
  if [ -n "$tail_pid" ]; then sleep 1; fi # 讓 tail 把最後幾行印完
  stop_tail
  restore
  # 成功就刪 log；中斷 / 逾時 / loadtest 失敗時保留，並把路徑印到 stderr
  if [ "$rc" = "0" ]; then
    rm -f "$log"
  elif [ -f "$log" ]; then
    echo "redis.sh: loadtest log kept at $log" >&2
  fi
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# loadtest 的輸出寫進 log（用來偵測 marker），再用背景 tail -f 即時顯示在終端機
: >"$log"
tail -f "$log" &
tail_pid=$!

start_ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# 容器內用目前使用者的 uid:gid 寫輸出（distroless 預設是 nonroot 65532，寫不進掛載目錄）
docker run --rm --user "$(id -u):$(id -g)" --network danmaku-fault_default --env-file "$here/.env" \
  -e TEST_DATABASE_URL='postgres://fault:fault@postgres:5432/danmaku_test?sslmode=disable' \
  -e TEST_REDIS_URL='redis://redis:6379/15' \
  -v "$here/.bin:/bin-loadtest:ro" -v "$out_dir:/out" \
  gcr.io/distroless/static-debian12:nonroot \
  /bin-loadtest/loadtest -in-compose -scenario "$scenario" -servers "$servers_arg" \
  -require-machine-check=false -docker-containers "" -out "/out/$out_name" "$@" >"$log" &
lt_pid=$!

# 每秒輪詢 log，等 loadtest 印出 marker（= 量測開始）；loadtest 先結束就不做 fault；逾時（以 wall-clock 計）就 kill loadtest 並失敗結束
wait_start="$(date +%s)"
seen=0
while :; do
  if grep -qF "$marker" "$log"; then seen=1; break; fi
  kill -0 "$lt_pid" 2>/dev/null || break
  if [ "$(($(date +%s) - wait_start))" -ge "$marker_timeout" ]; then
    echo "redis.sh: marker '$marker' not seen within ${marker_timeout}s, killing loadtest and skipping the fault" >&2
    exit 1 # on_exit 會 kill loadtest（並等它收尾）、停掉 tail、確認 Redis 是活的、保留 log
  fi
  sleep 1
done
if [ "$seen" = "1" ]; then sleep "$fault_offset"; fi

t_down="n/a"
t_up="n/a"
if [ "$seen" = "1" ] && kill -0 "$lt_pid" 2>/dev/null; then
  t_down="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  down=1
  docker compose -p danmaku-fault "$mode" redis
  sleep "$outage"
  if [ "$mode" = "stop" ]; then
    docker compose -p danmaku-fault start redis
  else
    docker compose -p danmaku-fault unpause redis
  fi
  down=0
  t_up="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
else
  echo "redis.sh: loadtest exited before the outage, skipping" >&2
fi

rc=0
wait "$lt_pid" || rc=$?
lt_pid=""
sleep 1 # 讓 tail 把最後幾行印完
stop_tail

printf 'run start: %s\nT_down (redis %s): %s\nT_up: %s\nloadtest exit code: %s\n' "$start_ts" "$mode" "$t_down" "$t_up" "$rc"

# 各種 log 行數（不是事件數：一個事件可能印多行）；logs 失敗就印 n/a；loadtest 失敗時也照印
for s in $targets; do
  if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" "$s" 2>/dev/null)"; then
    for pat in "publish to redis failed" "rate limiter failed" "pubsub judged deaf, rebuilding" "publish heartbeat failed" "subscribe room failed, will retry" "unsubscribe room failed" "create message failed"; do
      n="$(printf '%s\n' "$logs" | grep -c "$pat" || true)"
      printf 'log lines "%s" (%s, since %s): %s\n' "$pat" "$s" "$start_ts" "$n"
    done
  else
    printf 'log lines (%s, since %s): n/a (logs failed)\n' "$s" "$start_ts"
  fi
done
exit "$rc"
