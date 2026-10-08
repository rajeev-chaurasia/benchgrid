#!/usr/bin/env bash
# Deletes every VM labelled app=benchgrid, and with --all the GKE cluster too.
# The bucket, the dataset, the registry and the network are left: they cost
# close to nothing idle and hold the evidence. Written for the bash 3.2 that
# macOS ships, so no mapfile.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh
gc compute instances list --filter "labels.app=benchgrid AND labels.role=rig" --format "value(name,zone.basename())" |
  while read -r name zone; do
    [ -n "$name" ] && gc compute instances delete "$name" --zone "$zone" &
  done
wait
if [ "${1:-}" = "--all" ]; then
  gc container clusters delete "$CLUSTER" --zone "$ZONE" || true
  # Deleting a cluster leaves its persistent volumes behind as unattached
  # disks, which keep billing. This one did, once.
  gc compute disks list --filter "labels.app=benchgrid AND -users:*" --format "value(name,zone.basename())" |
    while read -r name zone; do
      [ -n "$name" ] && gc compute disks delete "$name" --zone "$zone"
    done
fi
echo "remaining benchgrid VMs:"
gc compute instances list --filter "labels.app=benchgrid" --format "value(name)"
