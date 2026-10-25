#!/usr/bin/env bash
# Start the stopped Offload node, refresh its (new) public IP, re-allow this
# machine's IP in the security group, and repoint the local CLI.
set -euo pipefail
cd "$(dirname "$0")/.."
aws() { env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN \
  AWS_PROFILE="${OFFLOAD_AWS_PROFILE:-offload}" AWS_REGION="${AWS_REGION:-ca-central-1}" command aws "$@"; }
ID="$(jq -r .instance_id ~/.offload/ec2.json)"
aws ec2 start-instances --instance-ids "$ID" >/dev/null
aws ec2 wait instance-running --instance-ids "$ID"
IP="$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)"
SG="$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].SecurityGroups[0].GroupId' --output text)"
MYIP="$(curl -fsS https://checkip.amazonaws.com)"
for port in 22 7070; do
  aws ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port $port --cidr "$MYIP/32" >/dev/null 2>&1 || true
done
jq --arg ip "$IP" '.ip = $ip' ~/.offload/ec2.json >~/.offload/ec2.json.tmp && mv ~/.offload/ec2.json.tmp ~/.offload/ec2.json
# Re-arm the 6-hour cost cap (instance-initiated shutdown = stop).
for i in $(seq 30); do ssh -i ~/.offload/offload-phase0.pem -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 "ubuntu@$IP" \
  "sudo shutdown -h +${OFFLOAD_TTL_MIN:-360} 'offload TTL' 2>/dev/null; systemctl is-active offload" && break; sleep 5; done
bin/offload init --node "https://$IP:7070" >/dev/null
bin/offload doctor
