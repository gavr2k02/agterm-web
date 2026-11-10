#!/bin/sh
# Claude Code SessionStart hook for agterm-web.
# Records which Claude transcript belongs to which agterm session pane, so the
# Telegram bot can mirror the conversation into the right topic. It only saves
# the hook's JSON input ({session_id, transcript_path, cwd, source, ...}) to
#   ~/.config/agterm-web/claude-sessions/<AGTERM_SESSION_ID>.<pane>.json
# and never fails or blocks Claude.

# Not running inside agterm: nothing to map.
[ -n "${AGTERM_SESSION_ID:-}" ] || { cat >/dev/null; exit 0; }

# The id becomes a file name: accept only a UUID-shaped value.
case "$AGTERM_SESSION_ID" in
  *[!0-9A-Fa-f-]* | "") cat >/dev/null; exit 0 ;;
esac
case "${AGTERM_PANE:-left}" in
  left | right | scratch) pane="${AGTERM_PANE:-left}" ;;
  *) pane=left ;;
esac

dir="$HOME/.config/agterm-web/claude-sessions"
mkdir -p "$dir" 2>/dev/null && chmod 700 "$dir" 2>/dev/null
f="$dir/$AGTERM_SESSION_ID.$pane.json"
tmp="$f.tmp.$$"
if head -c 65536 >"$tmp" 2>/dev/null; then
  chmod 600 "$tmp" 2>/dev/null
  mv -f "$tmp" "$f" 2>/dev/null
else
  rm -f "$tmp" 2>/dev/null
fi
exit 0
