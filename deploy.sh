#!/usr/bin/env bash
# Build goShelly for the Pi and install it as a systemd service.
#
#   ./deploy.sh [pi-host]
#
# Expects PI_PASS in the environment (or an SSH key already trusted by the Pi).
set -euo pipefail

PI_HOST="${1:-10.0.0.190}"
PI_USER="${PI_USER:-pi}"
REMOTE_DIR="${REMOTE_DIR:-/home/pi/goshelly}"

if [[ -z "${SHELLY_UI_PASSWORD:-}" ]]; then
  echo "SHELLY_UI_PASSWORD is not set. Export it before deploying, e.g." >&2
  echo "  SHELLY_UI_PASSWORD='your-password' ./deploy.sh ${PI_HOST}" >&2
  exit 1
fi
UI_PASSWORD="$SHELLY_UI_PASSWORD"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

if [[ -n "${PI_PASS:-}" ]]; then
  SSH=(sshpass -p "$PI_PASS" ssh -o StrictHostKeyChecking=no)
  SCP=(sshpass -p "$PI_PASS" scp -o StrictHostKeyChecking=no)
else
  SSH=(ssh -o StrictHostKeyChecking=no)
  SCP=(scp -o StrictHostKeyChecking=no)
fi

echo "==> building linux/arm64"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$staging/goshelly" "$here"

cp "$here/config.pi.json" "$staging/config.json"
printf 'SHELLY_UI_PASSWORD=%s\n' "$UI_PASSWORD" > "$staging/env"

cat > "$staging/goshelly.service" <<UNIT
[Unit]
Description=goShelly relay and GPIO button controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${PI_USER}
SupplementaryGroups=gpio
WorkingDirectory=${REMOTE_DIR}
EnvironmentFile=${REMOTE_DIR}/env
ExecStart=${REMOTE_DIR}/goshelly -config ${REMOTE_DIR}/config.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${REMOTE_DIR}
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT

echo "==> uploading to ${PI_USER}@${PI_HOST}:${REMOTE_DIR}"
"${SSH[@]}" "${PI_USER}@${PI_HOST}" "mkdir -p ${REMOTE_DIR}"
# The running binary cannot be overwritten in place (ETXTBSY), so stage it
# alongside and swap once the service is stopped.
mv "$staging/goshelly" "$staging/goshelly.new"
"${SCP[@]}" "$staging/goshelly.new" "$staging/config.json" "$staging/env" "$staging/goshelly.service" \
  "${PI_USER}@${PI_HOST}:${REMOTE_DIR}/"

echo "==> installing service"
"${SSH[@]}" "${PI_USER}@${PI_HOST}" "
  set -e
  chmod 600 ${REMOTE_DIR}/env
  sudo cp ${REMOTE_DIR}/goshelly.service /etc/systemd/system/goshelly.service
  sudo systemctl daemon-reload
  sudo systemctl stop goshelly 2>/dev/null || true
  mv ${REMOTE_DIR}/goshelly.new ${REMOTE_DIR}/goshelly
  chmod 755 ${REMOTE_DIR}/goshelly
  sudo systemctl enable goshelly
  sudo systemctl start goshelly
  sleep 2
  systemctl is-active goshelly
"

echo "==> done: http://${PI_HOST}:8080/  (user ${PI_USER:+shelly})"
