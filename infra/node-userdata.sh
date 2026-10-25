#!/bin/bash
# EC2 user-data for an Offload Phase 0 node (Ubuntu 24.04, x86_64).
set -eux
# Dead-man switch: instance-initiated shutdown stops the instance (cost cap; restart with ec2-start.sh).
shutdown -h +__TTL_MIN__ "offload phase0 TTL" || true

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -yq build-essential pkg-config libssl-dev git curl jq xfsprogs python3 python3-venv

# Workspaces live on a dedicated gp3 volume; XFS gives reflink copies for cheap workspace clones.
DEV=$(lsblk -dpno NAME,SERIAL | awk '$2 ~ /^vol/ {print $1}' | while read d; do
  [ -z "$(lsblk -no MOUNTPOINT "$d" | tr -d '[:space:]')" ] && [ -z "$(lsblk -no PARTTYPE "$d" | tr -d '[:space:]')" ] && echo "$d"; done | head -1)
mkfs.xfs -f -m reflink=1 "$DEV"
mkdir -p /data && mount -o noatime "$DEV" /data
echo "UUID=$(blkid -s UUID -o value "$DEV") /data xfs noatime,nofail 0 2" >>/etc/fstab
useradd -m -s /bin/bash offload
mkdir -p /data/offload && chown offload:offload /data/offload

# Toolchains (same versions as the developer laptop).
curl -fsSL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz | tar -C /usr/local -xz
sudo -iu offload bash -c 'curl -fsSL https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.98.1 --profile minimal'
sudo -iu offload bash -c 'curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash && . ~/.nvm/nvm.sh && nvm install 24.13.0 && npm i -g pnpm'
cat >/etc/profile.d/offload.sh <<'EOF'
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH
[ -s "$HOME/.nvm/nvm.sh" ] && . "$HOME/.nvm/nvm.sh" >/dev/null
EOF
touch /var/lib/offload-ready
