#!/bin/sh
# Install Server Manager on a systemd-based Linux host.
#
#   sudo ./scripts/install.sh dist/server-manager-linux-amd64
#
# Creates a dedicated unprivileged user, the directory layout below and the
# systemd unit. It does not start the service and never overwrites an
# existing configuration.
#
#   /usr/local/bin/server-manager          executable
#   /etc/server-manager/config.json        configuration
#   /etc/server-manager/servers/           one JSON file per server
#   /etc/server-manager/server-manager.env secrets (optional)
#   /var/lib/server-manager/               database, signing key, runtime files
set -eu

BIN="${1:-}"
if [ -z "$BIN" ] || [ ! -f "$BIN" ]; then
  echo "usage: $0 <path-to-server-manager-binary>" >&2
  exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "this installer must run as root" >&2
  exit 1
fi
HERE="$(cd "$(dirname "$0")/.." && pwd)"
SM_USER="${SM_USER:-server-manager}"

if ! id "$SM_USER" >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/server-manager --shell /usr/sbin/nologin "$SM_USER"
  echo "created user $SM_USER"
fi

install -m 0755 "$BIN" /usr/local/bin/server-manager
install -d -m 0750 -o "$SM_USER" -g "$SM_USER" /etc/server-manager /etc/server-manager/servers
install -d -m 0700 -o "$SM_USER" -g "$SM_USER" /var/lib/server-manager

if [ ! -f /etc/server-manager/config.json ]; then
  runuser -u "$SM_USER" -- /usr/local/bin/server-manager init --config /etc/server-manager/config.json
else
  echo "keeping existing /etc/server-manager/config.json"
  runuser -u "$SM_USER" -- /usr/local/bin/server-manager migrate --config /etc/server-manager/config.json
fi

if [ ! -f /etc/server-manager/server-manager.env ]; then
  install -m 0600 -o root -g root /dev/null /etc/server-manager/server-manager.env
  printf '# Secrets for Server Manager (mode 0600, read by systemd as root).\n# SM_SMTP_PASSWORD=...\n' > /etc/server-manager/server-manager.env
fi

install -m 0644 "$HERE/deploy/server-manager.service" /etc/systemd/system/server-manager.service
systemctl daemon-reload

cat <<MSG

Installed. Next steps:
  1. Edit /etc/server-manager/config.json (set "public_url", SMTP, ...)
  2. Create the first administrator:
       sudo -u $SM_USER server-manager admin create --username <name>
  3. Check the configuration:
       sudo -u $SM_USER server-manager check-config
  4. Start the service:
       sudo systemctl enable --now server-manager
  5. Put a TLS-terminating reverse proxy in front (see deploy/).
MSG
