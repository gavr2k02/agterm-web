#!/bin/sh
# Build agterm-web, install it to ~/.local/bin and run it at login via launchd.
# Re-run after changes: it rebuilds and restarts the service.
set -eu

cd "$(dirname "$0")"
LABEL=local.agterm-web
BIN="$HOME/.local/bin/agterm-web"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/agterm-web.log"
PORT="${AGTERM_WEB_PORT:-7681}"

go build -trimpath -o "$BIN.new" .
mv "$BIN.new" "$BIN"

mkdir -p "$HOME/Library/LaunchAgents"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN</string>
    <string>-addr</string><string>127.0.0.1:$PORT</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "agterm-web running on http://127.0.0.1:$PORT (log: $LOG)"
