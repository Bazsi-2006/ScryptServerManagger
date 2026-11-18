#!/bin/sh
# Backup script for a Minecraft server running in tmux.
#
# If the server is running, it uses the server console to make the backup
# consistent: "save-off" pauses world saving, "save-all flush" writes
# everything to disk, the worlds are archived, and "save-on" turns saving back
# on (also if anything goes wrong). If the server is stopped, the worlds are
# simply archived.
#
# Server Manager runs it when someone presses "Create backup" and sets
# SM_BACKUP_DIR to the server's configured backup directory.
set -eu

TMUX_SESSION="${TMUX_SESSION:-minecraft}"               # must match "tmux.session"
WORLDS="${WORLDS:-world world_nether world_the_end}"    # folders to back up
KEEP_BACKUPS="${KEEP_BACKUPS:-14}"                      # how many archives to keep
BACKUP_DIR="${SM_BACKUP_DIR:-$(dirname "$0")/backups}"
SAVE_TIMEOUT="${SAVE_TIMEOUT:-120}"                     # seconds to wait for "save-all"

cd "$(dirname "$0")"
LOG=logs/latest.log

running() { tmux has-session -t "=$TMUX_SESSION" 2>/dev/null; }

# Type one command into the server console (-l: literally, then Enter).
console() {
    tmux send-keys -t "=$TMUX_SESSION:" -l -- "$1" && tmux send-keys -t "=$TMUX_SESSION:" Enter
}

saves() { c=$(grep -c 'Saved the game' "$LOG" 2>/dev/null || true); echo "${c:-0}"; }

if running; then
    echo "[backup.sh] Server is running; flushing the world to disk"
    before=$(saves)
    # Whatever happens from here on, switch automatic saving back on.
    trap 'running && console "save-on"' EXIT
    console "save-off"
    console "save-all flush"
    waited=0
    while [ "$(saves)" -le "$before" ]; do
        if [ "$waited" -ge "$SAVE_TIMEOUT" ]; then
            echo "[backup.sh] ERROR: the server did not confirm 'Saved the game' within ${SAVE_TIMEOUT}s; no backup made"
            exit 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
else
    echo "[backup.sh] Server is not running; backing up the files as they are"
fi

present=""
for w in $WORLDS; do
    [ -d "$w" ] && present="$present $w"
done
[ -n "$present" ] || { echo "[backup.sh] ERROR: none of these folders exist: $WORLDS"; exit 1; }

mkdir -p "$BACKUP_DIR"
name="minecraft-$(date +%Y%m%d-%H%M%S).tar.gz"
# Write under a hidden temporary name first, so a half-written archive is
# never listed or downloaded.
# shellcheck disable=SC2086
tar -czf "$BACKUP_DIR/.$name.partial" $present
mv "$BACKUP_DIR/.$name.partial" "$BACKUP_DIR/$name"
echo "[backup.sh] Created $name ($(du -h "$BACKUP_DIR/$name" | cut -f1))"

# Delete the oldest archives beyond KEEP_BACKUPS.
ls -1t "$BACKUP_DIR"/minecraft-*.tar.gz 2>/dev/null | tail -n +"$((KEEP_BACKUPS + 1))" | while IFS= read -r old; do
    rm -f -- "$old"
    echo "[backup.sh] Removed old backup $(basename "$old")"
done

running && console "say Backup finished: $name" || true
