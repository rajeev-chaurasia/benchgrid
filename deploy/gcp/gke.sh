#!/usr/bin/env bash
# The control plane on a one-node zonal GKE cluster: Postgres as a
# StatefulSet and two benchgrid replicas behind an internal load balancer,
# which is how rig VMs reach it. The cluster's management fee is covered by
# GKE's free tier for one zonal cluster per billing account.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh
IMAGE=$(cat .image 2>/dev/null) || { echo "run ./images.sh first" >&2; exit 1; }

if ! gc container clusters describe "$CLUSTER" --zone "$ZONE" >/dev/null 2>&1; then
  gc container clusters create "$CLUSTER" --zone "$ZONE" \
    --network "$NETWORK" --subnetwork "$SUBNET" --enable-ip-alias \
    --cluster-secondary-range-name pods --services-secondary-range-name services \
    --num-nodes 1 --machine-type e2-standard-2 --disk-size 30 \
    --service-account "$NODE_SA" --scopes cloud-platform \
    --labels "$LABELS" --release-channel regular
fi
gc container clusters get-credentials "$CLUSTER" --zone "$ZONE"

# The database password is made here and lives only in the cluster's secret.
if ! kubectl get secret benchgrid-db >/dev/null 2>&1; then
  pw=$(openssl rand -hex 24)
  kubectl create secret generic benchgrid-db \
    --from-literal=password="$pw" \
    --from-literal=url="postgres://benchgrid:$pw@postgres:5432/benchgrid?sslmode=disable"
fi
kubectl apply -f k8s/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=300s
sed -e "s|IMAGE|$IMAGE|" -e "s|BUCKET|$BUCKET|" k8s/benchgrid.yaml | kubectl apply -f -
kubectl rollout status deployment/benchgrid --timeout=300s
for _ in $(seq 1 60); do
  ip=$(kubectl get svc benchgrid -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  [ -n "$ip" ] && break
  sleep 5
done
echo "control plane at http://$ip:8080 inside the VPC"
