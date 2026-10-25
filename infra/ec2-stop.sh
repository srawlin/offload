#!/usr/bin/env bash
# Stop (not terminate) the Offload node. Disks and caches survive; you pay only for EBS.
set -euo pipefail
aws() { env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN \
  AWS_PROFILE="${OFFLOAD_AWS_PROFILE:-offload}" AWS_REGION="${AWS_REGION:-ca-central-1}" command aws "$@"; }
ID="$(jq -r .instance_id ~/.offload/ec2.json)"
aws ec2 stop-instances --instance-ids "$ID" >/dev/null
aws ec2 wait instance-stopped --instance-ids "$ID"
echo "stopped $ID"
