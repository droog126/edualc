#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

mkdir -p .bin data/runtime
PID_FILE="data/runtime/hostexec.pid"
LOG_FILE="data/runtime/hostexec.log"
BIN=".bin/hostexec"

stop_old() {
  if [ -f "$PID_FILE" ]; then
    OLD_PID="$(cat "$PID_FILE" 2>/dev/null || true)"
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
      kill "$OLD_PID" 2>/dev/null || true
      sleep 0.3
    fi
    rm -f "$PID_FILE"
  fi
}

if [ "${1:-start}" = "stop" ]; then
  stop_old
  echo "[hostexec] 已停止。"
  exit 0
fi

stop_old
go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/hostexec

# Ensure host cargo/rustup/rg/agy/nvm(codex)/bun bin paths are included in PATH
NVM_BIN=""
if [ -d "$HOME/.nvm/versions/node" ]; then
  NVM_BIN="$(ls -d "$HOME"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -n 1 || true)"
fi
export PATH="${NVM_BIN:+${NVM_BIN}:}$HOME/.cargo/bin:$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
nohup "$BIN" >>"$LOG_FILE" 2>&1 &
NEW_PID="$!"
echo "$NEW_PID" > "$PID_FILE"

PORT="${HOSTEXEC_PORT:-8325}"
for i in $(seq 1 20); do
  if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    echo "[hostexec] 已启动 (PID=${NEW_PID}, 127.0.0.1:${PORT}, WORKSPACE=${WORKSPACE_DIR:-./workspace})"
    exit 0
  fi
  sleep 0.2
done

echo "[hostexec] 启动失败，查看日志：${LOG_FILE}" >&2
tail -n 20 "$LOG_FILE" >&2 || true
exit 1
