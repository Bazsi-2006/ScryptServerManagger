#!/bin/sh
# Example stop script: ask the server to shut down and wait for it.
set -eu
cd "$(dirname "$0")"
[ -f server.pid ] || { echo "not running"; exit 0; }
PID="$(cat server.pid)"
kill -TERM "$PID" 2>/dev/null || true
for _ in $(seq 1 30); do
  kill -0 "$PID" 2>/dev/null || { rm -f server.pid; echo "stopped"; exit 0; }
  sleep 1
done
echo "server did not stop in time" >&2
exit 1
