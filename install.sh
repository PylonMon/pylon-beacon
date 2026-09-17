#!/bin/sh
# pylon-beacon installer (Linux) — https://pylonmon.com/docs#beacon
# Usage:  curl -fsSL https://pylonmon.com/beacon.sh | sh
#         PYLON_KEY=pm_xxx curl -fsSL https://pylonmon.com/beacon.sh | sh   (non-interactive)
#         PYLON_TEMPLATE=db-servers picks which beacon template configures the node
set -e

REPO="PylonMon/pylon-beacon"
BIN="/usr/local/bin/pylon-beacon"
CONF="/etc/pylon-beacon.conf"
UNIT="/etc/systemd/system/pylon-beacon.service"

if [ "$(id -u)" != "0" ]; then
  echo "please run as root:  curl -fsSL https://pylonmon.com/beacon.sh | sudo sh"
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $(uname -m) — build from source: go build github.com/$REPO"; exit 1 ;;
esac

echo "-> downloading pylon-beacon (linux/$ARCH)…"
# Download beside the target and rename into place, rather than writing over
# $BIN directly. On Linux, opening a RUNNING executable for writing fails with
# ETXTBSY, so `curl -o $BIN` works on a first install and fails on every
# upgrade — the case that matters once the agent is deployed. A rename is
# atomic and is fine while the old binary is executing: the running process
# keeps its inode and picks up the new one at the restart below. It also means
# a download that dies halfway can never leave a truncated binary in place.
TMP="$BIN.new.$$"
trap 'rm -f "$TMP"' EXIT
curl -fsSL -o "$TMP" "https://github.com/$REPO/releases/latest/download/pylon-beacon-linux-$ARCH"
chmod +x "$TMP"
mv -f "$TMP" "$BIN"

if [ ! -f "$CONF" ]; then
  KEY="${PYLON_KEY:-}"
  if [ -z "$KEY" ]; then
    printf "PylonMon API key (ingest-scoped; Settings -> Admin -> Status page & API): "
    read -r KEY </dev/tty
  fi
  cat > "$CONF" <<EOF
# pylon-beacon — https://pylonmon.com/docs#beacon
key      = $KEY
url      = ${PYLON_URL:-https://pylonmon.com}
# The monitor is named after the machine. To use the full name instead,
# uncomment the next line (renaming keeps the same monitor — no duplicate):
# node   = $(hostname -f 2>/dev/null || hostname)
interval = 20
${PYLON_TEMPLATE:+template = $PYLON_TEMPLATE}

[custom]
# name = command   (first number in the output becomes the metric)
# gpu_temp_c = nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader
EOF
  chmod 600 "$CONF"
  echo "-> wrote $CONF"
else
  echo "-> keeping existing $CONF"
fi

cat > "$UNIT" <<EOF
[Unit]
Description=pylon-beacon — pushes node vitals to PylonMon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable pylon-beacon
# restart, not `enable --now`: --now STARTS a stopped unit but does nothing to
# one that is already running, so re-running this script to upgrade would leave
# the previous binary serving and report success.
systemctl restart pylon-beacon
echo ""
echo "✓ pylon-beacon is running. Your node appears in PylonMon within a minute."
echo "  status:  systemctl status pylon-beacon"
echo "  logs:    journalctl -u pylon-beacon -f"
echo "  config:  $CONF"
