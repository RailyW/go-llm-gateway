#!/usr/bin/env bash
# 本地起一套多实例，验证「转发横向扩展、控制与落库独立」这件事真的成立。
#
#   1 个 console :8080   管理台 + WebUI（不接转发流量）
#   2 个 gateway :8081 :8082   只转发（可以随便加）
#   1 个 worker         只消费日志队列 + 清理（选主）
#
# 用法：scripts/cluster.sh up | down | status | logs
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=./bin/gateway
RUN=.run/cluster
: "${GATEWAY_REDIS_ADDR:=127.0.0.1:6379}"
export GATEWAY_REDIS_ADDR

up() {
  [ -x "$BIN" ] || { echo "先 make build"; exit 1; }
  mkdir -p "$RUN"
  start() { # name role port
    local name=$1 role=$2 port=${3:-}
    GATEWAY_ROLE=$role GATEWAY_PORT=${port:-0} GATEWAY_INSTANCE_ID=$name \
      GATEWAY_DATA_DIR="$RUN/$name" \
      nohup "$BIN" > "$RUN/$name.log" 2>&1 &
    echo $! > "$RUN/$name.pid"
    printf '  %-10s role=%-8s %s\n' "$name" "$role" "${port:+http://localhost:$port}"
  }
  echo "启动多实例（Redis=$GATEWAY_REDIS_ADDR）:"
  start console console "${CONSOLE_PORT:-8080}"
  start gw-1    gateway "${GW1_PORT:-8081}"
  start gw-2    gateway "${GW2_PORT:-8082}"
  start worker  worker
  sleep 2
  echo "就绪。管理台 http://localhost:${CONSOLE_PORT:-8080}，转发入口 :${GW1_PORT:-8081} / :${GW2_PORT:-8082}"
}

down() {
  for f in "$RUN"/*.pid; do
    [ -f "$f" ] || continue
    kill "$(cat "$f")" 2>/dev/null || true
    rm -f "$f"
  done
  echo "已停止"
}

status() {
  for f in "$RUN"/*.pid; do
    [ -f "$f" ] || continue
    n=$(basename "$f" .pid)
    if kill -0 "$(cat "$f")" 2>/dev/null; then echo "  $n 运行中 (pid $(cat "$f"))"; else echo "  $n 已退出"; fi
  done
}

case "${1:-up}" in
  up) up ;;
  down) down ;;
  status) status ;;
  logs) tail -n 20 "$RUN"/*.log ;;
  *) echo "用法: $0 up|down|status|logs"; exit 1 ;;
esac
