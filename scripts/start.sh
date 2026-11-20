#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  cp .env.example .env
fi
mkdir -p workspace

./scripts/hostexec.sh start
docker compose up -d --build gateway relay desktop

PORT="$(grep -E '^HOST_PORT=' .env | cut -d= -f2 || true)"
PORT="${PORT:-8317}"
DPORT="$(grep -E '^DESKTOP_VNC_PORT=' .env | cut -d= -f2 || true)"
DPORT="${DPORT:-8318}"
TPORT="$(grep -E '^TERMINAL_PORT=' .env | cut -d= -f2 || true)"
TPORT="${TPORT:-8319}"

echo ""
echo "=========================================================="
echo "  edualc 已启动！"
echo "  控制台           : http://127.0.0.1:${PORT}/ui"
echo "  Claude 桌面端    : http://127.0.0.1:${DPORT}/edualc-desktop.html"
echo "  容器 Web 终端    : http://127.0.0.1:${TPORT}"
echo "  OpenAI API       : http://127.0.0.1:${PORT}/v1"
echo "  Anthropic API    : http://127.0.0.1:${PORT}"
echo "=========================================================="
