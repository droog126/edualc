#!/usr/bin/env bash
set -euo pipefail

ACCOUNT="${ACCOUNT:-default}"
ENV_SYNC="/data/runtime/env_sync.json"

TZ_VAL="UTC"
LANG_VAL="en_US.UTF-8"
LANGUAGE_ENV="en_US:en"
EXIT_IP="未就绪"

if [ -f "$ENV_SYNC" ]; then
  TZ_VAL="$(jq -r '.tz // "UTC"' "$ENV_SYNC")"
  LANG_VAL="$(jq -r '.lang // "en_US.UTF-8"' "$ENV_SYNC")"
  LANGUAGE_ENV="$(jq -r '.language_env // "en_US:en"' "$ENV_SYNC")"
  EXIT_IP="$(jq -r '.exit_ip // "未就绪"' "$ENV_SYNC")"
fi

ACC_DIR="/data/accounts/${ACCOUNT}"
mkdir -p "$ACC_DIR" /data/runtime/shims
# Auto-register Claude Code PreToolUse Bash hook for CLI account
CLI_SETTINGS="${ACC_DIR}/settings.json"
[ -f "$CLI_SETTINGS" ] || echo '{}' > "$CLI_SETTINGS"
jq '
  .hooks = (.hooks // {}) |
  .hooks.PreToolUse = (
    ((.hooks.PreToolUse // []) | map(select((.hooks // []) | all(.command != "/usr/local/bin/edualc-cmd-hook")))) +
    [{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/edualc-cmd-hook"}]}]
  )
' "$CLI_SETTINGS" > "${CLI_SETTINGS}.tmp" && mv "${CLI_SETTINGS}.tmp" "$CLI_SETTINGS" || true

export PATH="/data/runtime/shims:$PATH"
export HOME="$ACC_DIR"
export CLAUDE_CONFIG_DIR="$ACC_DIR"
export TZ="$TZ_VAL"
export LANG="$LANG_VAL"
export LC_ALL="$LANG_VAL"
export LANGUAGE="$LANGUAGE_ENV"
export HTTP_PROXY="${RELAY_PROXY_URL:-http://gateway:7890}"
export HTTPS_PROXY="${RELAY_PROXY_URL:-http://gateway:7890}"
export http_proxy="$HTTP_PROXY"
export https_proxy="$HTTPS_PROXY"
export NO_PROXY="localhost,127.0.0.1,gateway"
export no_proxy="localhost,127.0.0.1,gateway"
export HOSTEXEC_URL="${HOSTEXEC_URL:-http://gateway:18082/exec}"
export HOSTEXEC_TOKEN="${HOSTEXEC_TOKEN:-${INTERNAL_TOKEN:-}}"
export TMPDIR="/tmp/edualc"
mkdir -p /tmp/edualc
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export DISABLE_TELEMETRY=1
export DISABLE_ERROR_REPORTING=1
export DISABLE_AUTOUPDATER=1
export TERM="${TERM:-xterm-256color}"
export PS1='\[\e[38;5;173m\]edualc\[\e[0m\] \[\e[90m\][\w]\[\e[0m\] $ '

if [ -d /workspace ]; then
  cd /workspace
else
  cd "$ACC_DIR"
fi

# If explicit command args are passed (e.g. `edualc claude ...`), run them directly in this isolated env.
if [ "$#" -gt 0 ]; then
  exec "$@"
fi

echo "edualc 隔离终端 · 出口: ${EXIT_IP} · 时区: ${TZ} · 语言: ${LANG} · 账号: ${ACCOUNT} · 工作区: $(pwd)"
echo "提示：直接运行 claude 或 claude auth login（所有流量经隔离代理出口，零直连泄露）"
echo ""
exec /bin/bash --norc -i
