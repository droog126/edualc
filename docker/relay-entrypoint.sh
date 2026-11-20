#!/usr/bin/env bash
set -euo pipefail

# Start built-in Web Terminal (ttyd) on internal port 7681
ttyd -W -p 7681 \
  -t titleFixed=edualc \
  -t fontSize=13 \
  -t fontFamily='ui-monospace, SFMono-Regular, Menlo, Consolas, monospace' \
  -t copyOnSelect=true \
  -t macOptionIsMeta=true \
  -t disableLeaveAlert=true \
  -t 'theme={"background":"#181715","foreground":"#f1efe9","cursor":"#c96442","selectionBackground":"rgba(201,100,66,0.35)"}' \
  /usr/local/bin/airlock-shell.sh &

exec /usr/local/bin/relay
