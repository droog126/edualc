#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
./scripts/hostexec.sh stop || true
docker compose down
echo "[stop] edualc 已停止。"
