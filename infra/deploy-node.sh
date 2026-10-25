#!/usr/bin/env bash
# Build the Linux binary, install it on the EC2 node as a systemd service, and
# point the local offload CLI at it (~/.offload/config.json).
set -euo pipefail
cd "$(dirname "$0")/.."
IP="$(jq -r .ip ~/.offload/ec2.json)"
KEY=~/.offload/offload-phase0.pem
SSH=(ssh -i "$KEY" -o StrictHostKeyChecking=accept-new "ubuntu@$IP")

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/offload-linux-amd64 ./cmd/offload
scp -q -i "$KEY" bin/offload-linux-amd64 "ubuntu@$IP:/tmp/offload"
"${SSH[@]}" 'sudo bash -s' <<'EOF'
set -e
install -m 755 /tmp/offload /usr/local/bin/offload
cat >/etc/systemd/system/offload.service <<'UNIT'
[Unit]
Description=Offload node
After=network-online.target
[Service]
User=offload
# Login shell so PATH includes Go, cargo and nvm's node.
ExecStart=/bin/bash -lc 'exec /usr/local/bin/offload node --listen :7070 --root /data/offload'
Restart=on-failure
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable -q offload
systemctl restart offload
sleep 1
systemctl is-active offload
EOF
FP="$("${SSH[@]}" 'sudo journalctl -u offload --no-pager | grep -o "fingerprint [0-9a-f]*" | tail -1 | cut -d" " -f2')"
TOKEN="$("${SSH[@]}" 'sudo cat /data/offload/token')"
go build -o bin/offload ./cmd/offload
bin/offload init --node "https://$IP:7070" --token "$TOKEN" --fingerprint "$FP" >/dev/null
bin/offload doctor
