#!/usr/bin/env bash
set -euo pipefail

ACCOUNT="${1:-default}"
ENV_SYNC="/data/runtime/env_sync.json"

if [ ! -f "$ENV_SYNC" ]; then
  echo "[claude-login] 警告：尚未检测到同步后的出口环境 ($ENV_SYNC)。请先在控制台配置代理并通过检测！" >&2
  exit 1
fi

TZ_VAL="$(jq -r '.tz // empty' "$ENV_SYNC")"
LANG_VAL="$(jq -r '.lang // "en_US.UTF-8"' "$ENV_SYNC")"
LANGUAGE_ENV="$(jq -r '.language_env // "en_US:en"' "$ENV_SYNC")"
EXIT_IP="$(jq -r '.exit_ip // "unknown"' "$ENV_SYNC")"

if [ -n "$TZ_VAL" ] && [ -f "/usr/share/zoneinfo/${TZ_VAL}" ]; then
  ln -snf "/usr/share/zoneinfo/${TZ_VAL}" /etc/localtime
  echo "${TZ_VAL}" > /etc/timezone
fi

ACC_DIR="/data/accounts/${ACCOUNT}"
mkdir -p "$ACC_DIR"

export HOME="$ACC_DIR"
export CLAUDE_CONFIG_DIR="$ACC_DIR"
export TZ="${TZ_VAL:-UTC}"
export LANG="$LANG_VAL"
export LC_ALL="$LANG_VAL"
export LANGUAGE="$LANGUAGE_ENV"
export HTTP_PROXY="${RELAY_PROXY_URL:-http://gateway:7890}"
export HTTPS_PROXY="${RELAY_PROXY_URL:-http://gateway:7890}"
export http_proxy="$HTTP_PROXY"
export https_proxy="$HTTPS_PROXY"
export NO_PROXY="localhost,127.0.0.1"
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export DISABLE_TELEMETRY=1
export DISABLE_ERROR_REPORTING=1
export DISABLE_AUTOUPDATER=1

echo "============================================================"
echo "  账号槽位 : ${ACCOUNT} (${ACC_DIR})"
echo "  隔离出口 : ${EXIT_IP} (via ${HTTP_PROXY})"
echo "  同步环境 : TZ=${TZ}  LANG=${LANG}"
echo "  请复制下方授权链接，在隔离登录浏览器 (http://127.0.0.1:8318/vnc.html) 中打开并完成登录"
echo "============================================================"
exec claude auth login
