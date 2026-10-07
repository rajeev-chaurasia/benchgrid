#!/usr/bin/env bash
# gpu-region.sh REGION
# A subnet and NAT in a second region, for when no GPU is free in the first.
# The VPC is global, so a GPU node there reaches the control plane once the
# internal load balancer allows access from other regions.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh
R=$1
exists() { "$@" >/dev/null 2>&1; }
exists gc compute networks subnets describe "benchgrid-$R" --region "$R" ||
  gc compute networks subnets create "benchgrid-$R" --network "$NETWORK" --region "$R" \
    --range 10.41.0.0/20 --enable-private-ip-google-access
exists gc compute firewall-rules describe benchgrid-internal-gpu ||
  gc compute firewall-rules create benchgrid-internal-gpu --network "$NETWORK" \
    --allow tcp,udp,icmp --source-ranges 10.41.0.0/20
exists gc compute routers describe "benchgrid-router-$R" --region "$R" ||
  gc compute routers create "benchgrid-router-$R" --network "$NETWORK" --region "$R"
exists gc compute routers nats describe benchgrid-nat --router "benchgrid-router-$R" --region "$R" ||
  gc compute routers nats create benchgrid-nat --router "benchgrid-router-$R" --region "$R" \
    --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges
kubectl annotate svc benchgrid networking.gke.io/internal-load-balancer-allow-global-access=true --overwrite
echo "region $R ready"
