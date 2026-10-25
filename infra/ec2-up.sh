#!/usr/bin/env bash
# Launch a short-lived Offload node on EC2 for the Phase 0 speed proof.
#
#   infra/ec2-up.sh [instance-type]      (default c8i.8xlarge)
#
# Everything is tagged Project=offload-phase0 so ec2-down.sh can find it.
# The instance stops itself after OFFLOAD_TTL_MIN minutes (default 360)
# as a dead-man switch, in case SSO credentials expire before teardown.
set -euo pipefail

TYPE="${1:-c8i.8xlarge}"
REGION="${AWS_REGION:-ca-central-1}"
TTL_MIN="${OFFLOAD_TTL_MIN:-360}"
STATE="$HOME/.offload"
KEY_NAME="offload-phase0"
SG_NAME="offload-phase0"
TAG="Key=Project,Value=offload-phase0"
aws() { env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN \
  AWS_PROFILE="${OFFLOAD_AWS_PROFILE:-offload}" AWS_REGION="$REGION" command aws "$@"; }

mkdir -p "$STATE" && chmod 700 "$STATE"

if [[ ! -f "$STATE/$KEY_NAME.pem" ]]; then
  aws ec2 create-key-pair --key-name "$KEY_NAME" --key-type ed25519 \
    --tag-specifications "ResourceType=key-pair,Tags=[{$TAG}]" \
    --query KeyMaterial --output text >"$STATE/$KEY_NAME.pem"
  chmod 600 "$STATE/$KEY_NAME.pem"
fi

MYIP="$(curl -fsS https://checkip.amazonaws.com)"
VPC="$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)"
SG="$(aws ec2 describe-security-groups --filters Name=group-name,Values=$SG_NAME Name=vpc-id,Values=$VPC \
  --query 'SecurityGroups[0].GroupId' --output text)"
if [[ "$SG" == "None" ]]; then
  SG="$(aws ec2 create-security-group --group-name "$SG_NAME" --description "Offload phase0 node" \
    --vpc-id "$VPC" --tag-specifications "ResourceType=security-group,Tags=[{$TAG}]" --query GroupId --output text)"
fi
# SSH and the node API, from this machine's public IP only.
for port in 22 7070; do
  aws ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port $port \
    --cidr "$MYIP/32" >/dev/null 2>&1 || true
done

AMI="$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
  --query Parameter.Value --output text)"

USERDATA="$(sed "s/__TTL_MIN__/$TTL_MIN/" "$(dirname "$0")/node-userdata.sh")"

ID="$(aws ec2 run-instances --image-id "$AMI" --instance-type "$TYPE" --key-name "$KEY_NAME" \
  --security-group-ids "$SG" --instance-initiated-shutdown-behavior stop \
  --block-device-mappings '[
    {"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":40,"VolumeType":"gp3","DeleteOnTermination":true}},
    {"DeviceName":"/dev/sdf","Ebs":{"VolumeSize":300,"VolumeType":"gp3","Iops":16000,"Throughput":1000,"DeleteOnTermination":true}}]' \
  --tag-specifications "ResourceType=instance,Tags=[{$TAG},{Key=Name,Value=offload-node}]" \
    "ResourceType=volume,Tags=[{$TAG}]" \
  --user-data "$USERDATA" --query 'Instances[0].InstanceId' --output text)"
echo "instance: $ID ($TYPE)"
aws ec2 wait instance-running --instance-ids "$ID"
IP="$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)"
printf '{"instance_id":"%s","type":"%s","ip":"%s","region":"%s"}\n' "$ID" "$TYPE" "$IP" "$REGION" >"$STATE/ec2.json"
echo "ip: $IP  (state in $STATE/ec2.json; self-stops in ${TTL_MIN} min)"
