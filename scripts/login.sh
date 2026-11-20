#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

ACCOUNT="${1:-default}"
echo "[login] 确保隔离桌面环境就绪..."
docker compose up -d desktop >/dev/null
echo ">>> 请将下方授权链接复制到控制台「隔离浏览器」页（http://127.0.0.1:8317/ui）或 http://127.0.0.1:8320/edualc-desktop.html 完成登录"
echo ""
docker compose exec -it relay /usr/local/bin/claude-login "$ACCOUNT"
