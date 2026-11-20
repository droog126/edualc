#!/usr/bin/env bash
set -euo pipefail

ENV_SYNC="/data/runtime/env_sync.json"
if [ ! -f "$ENV_SYNC" ]; then
  echo "[login-browser] 拒绝启动：尚未检测到同步后的出口环境 ($ENV_SYNC)。请先在控制台配置代理并通过 10 项安全检测！" >&2
  exit 1
fi

TZ_VAL="$(jq -r '.tz // empty' "$ENV_SYNC")"
LANG_VAL="$(jq -r '.lang // "en_US.UTF-8"' "$ENV_SYNC")"
BCP47_VAL="$(jq -r '.bcp47 // "en-US"' "$ENV_SYNC")"
ACCEPT_LANG="$(jq -r '.accept_languages // "en-US,en"' "$ENV_SYNC")"
LANGUAGE_ENV="$(jq -r '.language_env // "en_US:en"' "$ENV_SYNC")"
EXIT_IP="$(jq -r '.exit_ip // "unknown"' "$ENV_SYNC")"

if [ -z "$TZ_VAL" ] || [ ! -f "/usr/share/zoneinfo/${TZ_VAL}" ]; then
  echo "[login-browser] 拒绝启动：出口时区无效 (${TZ_VAL})。" >&2
  exit 1
fi

# System-level timezone sync inside login container
ln -snf "/usr/share/zoneinfo/${TZ_VAL}" /etc/localtime
echo "${TZ_VAL}" > /etc/timezone

export TZ="$TZ_VAL"
export LANG="$LANG_VAL"
export LC_ALL="$LANG_VAL"
export LANGUAGE="$LANGUAGE_ENV"
export DISPLAY=:99

# Clean ephemeral Chromium profile with Accept-Language & selected_languages pre-seeded to match exit country
PROFILE_DIR="/tmp/chrome-profile"
rm -rf "$PROFILE_DIR"
mkdir -p "${PROFILE_DIR}/Default"
jq -n \
  --arg al "$ACCEPT_LANG" \
  '{
    "intl": {
      "accept_languages": $al,
      "selected_languages": $al
    },
    "webrtc": {
      "ip_handling_policy": "disable_non_proxied_udp"
    }
  }' > "${PROFILE_DIR}/Default/Preferences"

echo "[login-browser] starting isolated Chromium:"
echo "  EXIT_IP         = ${EXIT_IP}"
echo "  TZ              = ${TZ}"
echo "  LANG / LANGUAGE = ${LANG} / ${LANGUAGE}"
echo "  BCP47 / ACCEPT  = ${BCP47_VAL} / ${ACCEPT_LANG}"
echo "  PROXY           = ${PROXY_URL:-http://gateway:7890}"

Xvfb :99 -screen 0 1280x840x24 -nolisten tcp &
sleep 1
openbox &
x11vnc -display :99 -forever -shared -nopw -rfbport 5900 -listen 127.0.0.1 &
websockify --web /usr/share/novnc 0.0.0.0:6080 127.0.0.1:5900 &

exec chromium \
  --no-sandbox \
  --disable-dev-shm-usage \
  --disable-gpu \
  --no-first-run \
  --disable-sync \
  --user-data-dir="${PROFILE_DIR}" \
  --window-size=1280,820 \
  --window-position=0,0 \
  --lang="${BCP47_VAL}" \
  --accept-lang="${ACCEPT_LANG}" \
  --proxy-server="${PROXY_URL:-http://gateway:7890}" \
  --proxy-bypass-list="127.0.0.1;localhost" \
  --force-webrtc-ip-handling-policy=disable_non_proxied_udp \
  "https://claude.ai"
