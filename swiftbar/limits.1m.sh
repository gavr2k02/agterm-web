#!/bin/sh
# <xbar.title>Claude / Codex limits</xbar.title>
# <xbar.desc>Usage limits of the cswap Claude accounts and Codex, from agterm-web.</xbar.desc>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
#
# The menu is rendered by the local binary itself (no web server, no token):
#   agterm-web -limits-swiftbar
# Another binary path: set AGTERM_WEB_BIN.
BIN="${AGTERM_WEB_BIN:-$HOME/.local/bin/agterm-web}"
if out=$("$BIN" -limits-swiftbar 2>/dev/null) && [ -n "$out" ]; then
  printf '%s\n' "$out"
else
  echo "Claude ? · Codex ? | color=#ff5a5a"
  echo "---"
  echo "Не удалось получить лимиты | color=#ff5a5a"
  echo "Нужен $BIN с флагом -limits-swiftbar (./install.sh) | size=11"
  echo "Обновить | refresh=true"
fi
