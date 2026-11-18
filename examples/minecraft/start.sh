#!/bin/sh
# Minecraft start script for Server Manager (tmux backend).
#
# Server Manager runs this script inside a tmux session, as the gameserver
# user:
#
#   tmux session "minecraft"   (owned by the Server Manager service user)
#     -> sudo -n -u gameserver /home/gameserver/minecraft/start.sh
#          -> java ... -jar server.jar nogui        (runs as gameserver)
#
# The script stays in the foreground. When Minecraft stops, the script ends,
# the tmux session closes, and Server Manager shows the server as stopped.
# Do not add "&", screen, tmux or a restart loop here.
#
# Install (as root):
#   install -m 0750 -o gameserver -g gameserver start.sh /home/gameserver/minecraft/start.sh
# The Server Manager service user must NOT be able to modify this file.

set -eu

# ------------------------------------------------------------------ settings
SERVER_DIR="/home/gameserver/minecraft"  # folder with the server jar and world
SERVER_JAR="server.jar"                  # e.g. paper.jar, fabric-server-launch.jar
JAVA="/usr/bin/java"                     # full path: sudo resets PATH
MIN_MEMORY="2G"
MAX_MEMORY="4G"
RUN_AS="gameserver"                      # refuse to run as any other user
# Extra JVM options. These are safe defaults; leave them if unsure.
JAVA_OPTS="-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200"
# ---------------------------------------------------------------------------

fail() {
	echo "[start.sh] ERROR: $*" >&2
	exit 1
}

[ "$(id -un)" = "$RUN_AS" ] ||
	fail "must run as user '$RUN_AS', not '$(id -un)'. Start it with: sudo -u $RUN_AS $0"
cd "$SERVER_DIR" || fail "cannot enter $SERVER_DIR"
[ -x "$JAVA" ] || fail "Java was not found at $JAVA (install it, or fix JAVA= in this script)"
[ -f "$SERVER_JAR" ] || fail "$SERVER_DIR/$SERVER_JAR does not exist"
grep -qi '^eula=true' eula.txt 2>/dev/null ||
	fail "the Minecraft EULA is not accepted: read https://aka.ms/MinecraftEULA and set eula=true in $SERVER_DIR/eula.txt"
command -v flock >/dev/null 2>&1 || fail "flock is missing (it is part of the util-linux package)"

# Only one copy may use this world at a time. flock keeps the lock for as
# long as Java runs; this check gives a readable message if it is taken.
LOCK="$SERVER_DIR/.server.lock"
flock -n "$LOCK" true || fail "this server is already running (lock $LOCK is held)"

umask 027
echo "[start.sh] starting $SERVER_JAR as $(id -un) in $SERVER_DIR (memory $MIN_MEMORY to $MAX_MEMORY)"

# shellcheck disable=SC2086 # JAVA_OPTS is split into separate options on purpose
exec flock -n "$LOCK" "$JAVA" -Xms"$MIN_MEMORY" -Xmx"$MAX_MEMORY" $JAVA_OPTS -jar "$SERVER_JAR" nogui
