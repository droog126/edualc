#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

ACCOUNT="${1:-default}"
echo "[login] 确保隔离桌面环境就绪..."
docker compose up -d desktop >/dev/null
echo ">>> 可在控制台「Claude 桌面端」页右键打开隔离浏览器，或访问 http://127.0.0.1:8318/vnc.html?autoconnect=true&resize=remote"
echo ""
docker compose exec -it relay /usr/local/bin/claude-login "$ACCOUNT"
