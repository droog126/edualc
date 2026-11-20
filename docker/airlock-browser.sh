#!/usr/bin/env bash
set -euo pipefail

ENV_SYNC="/data/runtime/env_sync.json"
TZ_VAL="${TZ:-UTC}"
LANG_VAL="${LANG:-en_US.UTF-8}"
BCP47_VAL="en-US"
ACCEPT_LANG="en-US,en"
LANGUAGE_ENV="${LANGUAGE:-en_US:en}"

if [ -f "$ENV_SYNC" ]; then
  TZ_VAL="$(jq -r '.tz // "UTC"' "$ENV_SYNC")"
  LANG_VAL="$(jq -r '.lang // "en_US.UTF-8"' "$ENV_SYNC")"
  BCP47_VAL="$(jq -r '.bcp47 // "en-US"' "$ENV_SYNC")"
  ACCEPT_LANG="$(jq -r '.accept_languages // "en-US,en"' "$ENV_SYNC")"
  LANGUAGE_ENV="$(jq -r '.language_env // "en_US:en"' "$ENV_SYNC")"
fi

# Always route all external URLs (including clicks from Claude Desktop :78) into the dedicated Isolated Browser (:79)
export DISPLAY=":79"
export TZ="$TZ_VAL"
export LANG="$LANG_VAL"
export LC_ALL="$LANG_VAL"
export LANGUAGE="$LANGUAGE_ENV"

PROFILE_DIR="/data/desktop/.config/airlock-chromium-79"
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

TARGET_URL="${1:-https://claude.ai}"
jq -n --arg url "$TARGET_URL" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{"url":$url,"ts":$ts}' > /data/runtime/browser_last_opened.json 2>/dev/null || true

exec chromium \
  --no-sandbox \
  --disable-dev-shm-usage \
  --disable-gpu \
  --no-first-run \
  --disable-sync \
  --user-data-dir="${PROFILE_DIR}" \
  --lang="${BCP47_VAL}" \
  --accept-lang="${ACCEPT_LANG}" \
  --proxy-server="${PROXY_URL:-http://gateway:7890}" \
  --proxy-bypass-list="127.0.0.1;localhost" \
  --force-webrtc-ip-handling-policy=disable_non_proxied_udp \
  "$TARGET_URL"
