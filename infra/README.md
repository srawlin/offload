# EC2 node scripts

These are the scripts that ran the Phase 0 benchmark node. They're an example of a disposable,
pay-by-the-hour node, not a production setup.

```sh
infra/ec2-up.sh c8i.8xlarge   # launch Ubuntu 24.04 + toolchains + an XFS data volume; self-stops after 6 h
infra/deploy-node.sh          # install the node as a systemd service and run `offload init` locally
infra/ec2-stop.sh             # stop: disks and warm caches survive, compute billing stops
infra/ec2-start.sh            # start again: refreshes the IP, security group rule and CLI config
infra/ec2-down.sh             # DESTRUCTIVE: terminate and delete the security group and key pair
```

- AWS calls use the CLI profile `offload` (override with `OFFLOAD_AWS_PROFILE`) and region
  `ca-central-1` (override with `AWS_REGION`).
- The security group allows ports 22 and 7070 from your current public IP only.
- State (SSH key, instance id) is kept in `~/.offload/`.
- A stopped instance still pays for its EBS volumes. The data volume is created with provisioned
  IOPS and throughput (16,000 / 1,000 MB/s), which cost money even while the instance is stopped.
  Lower them in `ec2-up.sh`, or with `aws ec2 modify-volume`, if you keep a node around.
