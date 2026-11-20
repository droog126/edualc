#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="8317"
API_KEY=""
if [ -f .env ]; then
  P="$(grep -E '^HOST_PORT=' .env | cut -d= -f2 || true)"
  K="$(grep -E '^RELAY_API_KEY=' .env | cut -d= -f2 || true)"
  [ -n "$P" ] && PORT="$P"
  [ -n "$K" ] && API_KEY="$K"
fi

PROMPT="${1:-你好，请只回复 OK。}"
MODEL="${2:-sonnet}"

echo "[test] 检查安全门控状态 (/health):"
curl -sS "http://127.0.0.1:${PORT}/health"
echo ""
echo ""
echo "[test] 发送一句话测试 (/v1/chat/completions):"
curl -sS "http://127.0.0.1:${PORT}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${API_KEY}" \
  -d "{\"model\":\"${MODEL}\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"${PROMPT}\"}]}"
echo ""
