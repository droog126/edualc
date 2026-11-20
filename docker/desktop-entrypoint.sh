#!/usr/bin/env bash
set -euo pipefail

ENV_SYNC="/data/runtime/env_sync.json"

echo "[desktop] 等待安全检测与出口环境同步 ($ENV_SYNC)..."
while [ ! -f "$ENV_SYNC" ]; do
  sleep 2
done

TZ_VAL="$(jq -r '.tz // "UTC"' "$ENV_SYNC")"
LANG_VAL="$(jq -r '.lang // "en_US.UTF-8"' "$ENV_SYNC")"
BCP47_VAL="$(jq -r '.bcp47 // "en-US"' "$ENV_SYNC")"
ACCEPT_LANG="$(jq -r '.accept_languages // "en-US,en"' "$ENV_SYNC")"
LANGUAGE_ENV="$(jq -r '.language_env // "en_US:en"' "$ENV_SYNC")"
EXIT_IP="$(jq -r '.exit_ip // "unknown"' "$ENV_SYNC")"

if [ -n "$TZ_VAL" ] && [ -f "/usr/share/zoneinfo/${TZ_VAL}" ]; then
  ln -snf "/usr/share/zoneinfo/${TZ_VAL}" /etc/localtime
  echo "${TZ_VAL}" > /etc/timezone
fi

mkdir -p /data/desktop/.local/share/keyrings /data/desktop/.config/fcitx5 /data/desktop/.claude /data/runtime/shims /tmp/edualc /run/airlock-desktop
# Auto-register Claude Code PreToolUse Bash hook to capture unproxied commands
SETTINGS_FILE="/data/desktop/.claude/settings.json"
[ -f "$SETTINGS_FILE" ] || echo '{}' > "$SETTINGS_FILE"
jq '
  .hooks = (.hooks // {}) |
  .hooks.PreToolUse = (
    ((.hooks.PreToolUse // []) | map(select((.hooks // []) | all(.command != "/usr/local/bin/edualc-cmd-hook")))) +
    [{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/edualc-cmd-hook"}]}]
  )
' "$SETTINGS_FILE" > "${SETTINGS_FILE}.tmp" && mv "${SETTINGS_FILE}.tmp" "$SETTINGS_FILE" || true
# Publish installed desktop versions for the relay update checker
CD_VER="$(dpkg-query -W -f='${Version}' claude-desktop 2>/dev/null || echo '')"
CR_VER="$(chromium --version 2>/dev/null | head -n1 || echo '')"
jq -n --arg cd "$CD_VER" --arg cr "$CR_VER" '{"claude_desktop":$cd,"chromium":$cr}' > /data/runtime/desktop_versions.json 2>/dev/null || true
if [ -d /workspace ] && [ ! -e /data/desktop/workspace ]; then
  ln -s /workspace /data/desktop/workspace
fi

# Seed fcitx5 profile (English + Chinese Pinyin, toggle via Ctrl+Space) if not customized yet
if [ ! -f /data/desktop/.config/fcitx5/profile ]; then
  cat > /data/desktop/.config/fcitx5/profile <<'EOF'
[Groups/0]
Name=Default
Default Layout=us
DefaultIM=keyboard-us

[Groups/0/Items/0]
Name=keyboard-us
Layout=

[Groups/0/Items/1]
Name=pinyin
Layout=

[GroupOrder]
0=Default
EOF
fi

chown -R airlock:airlock /data/desktop /run/airlock-desktop
chmod 700 /run/airlock-desktop

cat > /usr/local/bin/airlock-claude-desktop <<EOF
#!/usr/bin/env bash
export PATH="/data/runtime/shims:\$PATH"
export TZ="${TZ_VAL}"
export LANG="${LANG_VAL}"
export LC_ALL="${LANG_VAL}"
export LANGUAGE="${LANGUAGE_ENV}"
export GTK_IM_MODULE=fcitx
export QT_IM_MODULE=fcitx
export XMODIFIERS=@im=fcitx
export INPUT_METHOD=fcitx
export HTTP_PROXY="${PROXY_URL:-http://gateway:7890}"
export HTTPS_PROXY="${PROXY_URL:-http://gateway:7890}"
export NO_PROXY="localhost,127.0.0.1,gateway"
export HOSTEXEC_URL="${HOSTEXEC_URL:-http://gateway:18082/exec}"
export HOSTEXEC_TOKEN="${HOSTEXEC_TOKEN:-}"
export TMPDIR="/tmp/edualc"
export BROWSER="/usr/local/bin/airlock-browser"
exec claude-desktop \\
  --no-sandbox \\
  --disable-gpu-sandbox \\
  --disable-dev-shm-usage \\
  --proxy-server="${PROXY_URL:-http://gateway:7890}" \\
  --proxy-bypass-list="127.0.0.1;localhost" \\
  --lang="${BCP47_VAL}" \\
  --password-store=gnome-libsecret
EOF
chmod +x /usr/local/bin/airlock-claude-desktop

echo "[desktop] 启动 edualc 双画布（:78 Claude Desktop + :79 隔离 Chromium）: EXIT_IP=${EXIT_IP} TZ=${TZ_VAL} LANG=${LANG_VAL}"

exec su -s /bin/bash airlock -c "
  set -euo pipefail
  export PATH="/data/runtime/shims:\$PATH"
  export HOME=/data/desktop
  export XDG_RUNTIME_DIR=/run/airlock-desktop
  export XDG_CURRENT_DESKTOP=Openbox
  export DISPLAY=:78
  export TZ='${TZ_VAL}'
  export LANG='${LANG_VAL}'
  export LC_ALL='${LANG_VAL}'
  export LANGUAGE='${LANGUAGE_ENV}'
  export GTK_IM_MODULE=fcitx
  export QT_IM_MODULE=fcitx
  export XMODIFIERS=@im=fcitx
  export INPUT_METHOD=fcitx
  export HTTP_PROXY='${PROXY_URL:-http://gateway:7890}'
  export HTTPS_PROXY='${PROXY_URL:-http://gateway:7890}'
  export NO_PROXY='localhost,127.0.0.1,gateway'
  export HOSTEXEC_URL='${HOSTEXEC_URL:-http://gateway:18082/exec}'
  export HOSTEXEC_TOKEN='${HOSTEXEC_TOKEN:-}'
  export TMPDIR='/tmp/edualc'
  export BROWSER='/usr/local/bin/airlock-browser'

  exec dbus-run-session -- bash -c '
    set -euo pipefail

    # ---- Display :78 -> Official Claude Desktop (websockify :6080 -> 5978) ----
    Xtigervnc :78 -geometry 1440x900 -depth 24 -rfbport 5978 -localhost yes -SecurityTypes None -AlwaysShared &
    for i in \$(seq 1 60); do
      xdpyinfo -display :78 >/dev/null 2>&1 && break
      sleep 0.2
    done
    DISPLAY=:78 openbox --config-file /etc/airlock/openbox-rc.xml &
    DISPLAY=:78 autocutsel -fork &
    DISPLAY=:78 autocutsel -selection PRIMARY -fork &
    DISPLAY=:78 fcitx5 -d >/dev/null 2>&1 || true

    # ---- Display :79 -> Dedicated Isolated Chromium Browser (websockify :6081 -> 5979) ----
    Xtigervnc :79 -geometry 1440x900 -depth 24 -rfbport 5979 -localhost yes -SecurityTypes None -AlwaysShared &
    for i in \$(seq 1 60); do
      xdpyinfo -display :79 >/dev/null 2>&1 && break
      sleep 0.2
    done
    DISPLAY=:79 openbox --config-file /etc/airlock/openbox-rc.xml &
    DISPLAY=:79 autocutsel -fork &
    DISPLAY=:79 autocutsel -selection PRIMARY -fork &

    sleep 0.5
    printf \"%s\\n\" \"airlock-local-keyring\" | gnome-keyring-daemon --unlock --components=secrets >/dev/null 2>&1 || true

    websockify --web /usr/share/novnc 0.0.0.0:6080 127.0.0.1:5978 &
    websockify --web /usr/share/novnc 0.0.0.0:6081 127.0.0.1:5979 &
    python3 /usr/local/bin/clipboard-bridge.py &

    # Keep isolated Chromium running on Display :79
    (
      while true; do
        DISPLAY=:79 /usr/local/bin/airlock-browser https://claude.ai || true
        sleep 2
      done
    ) &

    # Watch URL open requests from Web UI (POST /admin/browser/open)
    (
      TRIGGER=\"/data/runtime/browser_open_url\"
      while true; do
        if [ -s \"\$TRIGGER\" ]; then
          TARGET=\"\$(head -n 1 \"\$TRIGGER\" | tr -d \"\\r\\n\")\"
          rm -f \"\$TRIGGER\" || true
          if [ -n \"\$TARGET\" ]; then
            DISPLAY=:79 /usr/local/bin/airlock-browser \"\$TARGET\" >/dev/null 2>&1 &
          fi
        fi
        sleep 0.4
      done
    ) &

    # Keep Claude Desktop running on Display :78
    while true; do
      DISPLAY=:78 /usr/local/bin/airlock-claude-desktop || true
      sleep 2
    done
  '
"
