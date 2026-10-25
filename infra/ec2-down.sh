#!/usr/bin/env bash
# DESTRUCTIVE: terminate every Offload Phase 0 instance (deletes its disks and caches) and delete its security group and key pair.
set -euo pipefail
REGION="${AWS_REGION:-ca-central-1}"
aws() { env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN \
  AWS_PROFILE="${OFFLOAD_AWS_PROFILE:-offload}" AWS_REGION="$REGION" command aws "$@"; }

IDS="$(aws ec2 describe-instances --filters Name=tag:Project,Values=offload-phase0 \
  Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].InstanceId' --output text)"
if [[ -n "$IDS" ]]; then
  echo "terminating: $IDS"
  aws ec2 terminate-instances --instance-ids $IDS >/dev/null
  aws ec2 wait instance-terminated --instance-ids $IDS
fi
SG="$(aws ec2 describe-security-groups --filters Name=group-name,Values=offload-phase0 \
  --query 'SecurityGroups[0].GroupId' --output text)"
[[ "$SG" != "None" ]] && aws ec2 delete-security-group --group-id "$SG" && echo "deleted sg $SG"
aws ec2 delete-key-pair --key-name offload-phase0 >/dev/null 2>&1 && echo "deleted key pair"
rm -f "$HOME/.offload/offload-phase0.pem" "$HOME/.offload/ec2.json"
echo done
