#!/bin/sh
# Example status script.
#   exit 0  = ready / running
#   exit 1  = not ready / not running
#   other   = the check itself failed (shown as UNKNOWN)
cd "$(dirname "$0")" || exit 2
[ -f server.pid ] || exit 1
kill -0 "$(cat server.pid)" 2>/dev/null || exit 1
# Running, but is it accepting connections yet?
if command -v nc >/dev/null 2>&1; then
  nc -z 127.0.0.1 "${SERVER_PORT:-27015}" >/dev/null 2>&1 || exit 1
fi
exit 0
