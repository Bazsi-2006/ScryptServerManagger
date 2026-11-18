#!/bin/sh
# Start script for a Minecraft Java Edition server (vanilla, Paper, Purpur,
# Fabric, ...), meant to run INSIDE a tmux session.
#
# With Server Manager's tmux backend you do not start tmux yourself: Server
# Manager creates the session and runs this script in it. The script replaces
# itself with Java ("exec"), so when Minecraft stops the session ends and
# Server Manager sees the server as stopped.
#
# You can also use it by hand:
#   tmux new-session -d -s minecraft /opt/minecraft/start.sh
#
# Settings come from environment variables, which you can set in the server
# definition's "env" section. The defaults are below.
set -eu

SERVER_JAR="${SERVER_JAR:-server.jar}"   # the server jar, relative to this folder
MIN_MEMORY="${MIN_MEMORY:-2G}"           # -Xms
MAX_MEMORY="${MAX_MEMORY:-4G}"           # -Xmx (leave room for the OS!)
JAVA_BIN="${JAVA_BIN:-java}"             # e.g. /usr/lib/jvm/java-21-openjdk-amd64/bin/java
EXTRA_JAVA_ARGS="${EXTRA_JAVA_ARGS:-}"   # additional JVM flags, space separated

# Always run from the server folder, wherever we were started from.
cd "$(dirname "$0")"

fail() {
    echo "[start.sh] ERROR: $*"
    # Keep the message on screen for a moment: if the script exited at once,
    # the tmux session would disappear before anyone could read it.
    sleep 5
    exit 1
}

command -v "$JAVA_BIN" >/dev/null 2>&1 \
    || fail "Java was not found ('$JAVA_BIN'). Install a Java runtime (Minecraft 1.20.5+ needs Java 21) or set JAVA_BIN."

[ -f "$SERVER_JAR" ] \
    || fail "Server jar '$SERVER_JAR' not found in $(pwd). Download it there or set SERVER_JAR."

# Minecraft refuses to start until the EULA is accepted. Accepting it is your
# decision, so the script only explains what to do.
if ! grep -qi '^eula=true' eula.txt 2>/dev/null; then
    fail "The Minecraft EULA has not been accepted. Read https://aka.ms/MinecraftEULA, then run: echo eula=true > $(pwd)/eula.txt"
fi

echo "[start.sh] $("$JAVA_BIN" -version 2>&1 | head -n 1)"
echo "[start.sh] Starting $SERVER_JAR with $MIN_MEMORY-$MAX_MEMORY of memory in $(pwd)"

# EXTRA_JAVA_ARGS is deliberately unquoted so it splits into separate flags.
# shellcheck disable=SC2086
exec "$JAVA_BIN" -Xms"$MIN_MEMORY" -Xmx"$MAX_MEMORY" -XX:+UseG1GC $EXTRA_JAVA_ARGS \
    -jar "$SERVER_JAR" nogui
