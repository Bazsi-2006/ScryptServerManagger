#!/bin/sh
# Example start script for a "forking" custom server.
# It launches the server in the background, records its PID and exits.
# Server Manager does not assume the server stopped when this script ends.
set -eu
cd "$(dirname "$0")"
mkdir -p logs
nohup ./server-binary "$@" >> logs/latest.log 2>&1 &
echo $! > server.pid
echo "started with PID $(cat server.pid)"
