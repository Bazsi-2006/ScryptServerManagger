#!/bin/sh
# Example backup script. Server Manager sets:
#   SM_SERVER_ID   the server's ID
#   SM_BACKUP_DIR  the configured backup directory
# Write the archive under a temporary name and rename it when complete, so a
# half-written file is never offered for download.
set -eu
cd "$(dirname "$0")"
mkdir -p "$SM_BACKUP_DIR"
NAME="${SM_SERVER_ID}-$(date +%Y%m%d-%H%M%S).tar.gz"
tar -czf "$SM_BACKUP_DIR/.$NAME.partial" world
mv "$SM_BACKUP_DIR/.$NAME.partial" "$SM_BACKUP_DIR/$NAME"
# Keep the 14 most recent backups.
ls -1t "$SM_BACKUP_DIR"/*.tar.gz 2>/dev/null | tail -n +15 | xargs -r rm -f
echo "created $NAME"
