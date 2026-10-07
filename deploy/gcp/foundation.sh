#!/usr/bin/env bash
# One-time, and cheap to leave in place: the VPC, NAT for VMs without public
# addresses, the run store bucket, the BigQuery dataset, and the image
# registry. Re-running it skips whatever already exists.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh

gc services enable compute.googleapis.com container.googleapis.com artifactregistry.googleapis.com \
  bigquery.googleapis.com storage.googleapis.com iap.googleapis.com

exists() { "$@" >/dev/null 2>&1; }

exists gc compute networks describe "$NETWORK" ||
  gc compute networks create "$NETWORK" --subnet-mode=custom
# Pods and services get secondary ranges so GKE is VPC native, which lets rig
# VMs and control plane pods reach each other by address with no proxy.
exists gc compute networks subnets describe "$SUBNET" --region "$REGION" ||
  gc compute networks subnets create "$SUBNET" --network "$NETWORK" --region "$REGION" \
    --range 10.40.0.0/20 --secondary-range pods=10.44.0.0/14,services=10.48.0.0/20 \
    --enable-private-ip-google-access
exists gc compute firewall-rules describe benchgrid-internal ||
  gc compute firewall-rules create benchgrid-internal --network "$NETWORK" \
    --allow tcp,udp,icmp --source-ranges 10.40.0.0/20,10.44.0.0/14
# SSH only through Identity-Aware Proxy: no VM here has a public address.
exists gc compute firewall-rules describe benchgrid-iap-ssh ||
  gc compute firewall-rules create benchgrid-iap-ssh --network "$NETWORK" \
    --allow tcp:22 --source-ranges 35.235.240.0/20
exists gc compute routers describe benchgrid-router --region "$REGION" ||
  gc compute routers create benchgrid-router --network "$NETWORK" --region "$REGION"
exists gc compute routers nats describe benchgrid-nat --router benchgrid-router --region "$REGION" ||
  gc compute routers nats create benchgrid-nat --router benchgrid-router --region "$REGION" \
    --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges

exists gcloud storage buckets describe "gs://$BUCKET" --project "$PROJECT" ||
  gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" \
    --uniform-bucket-level-access --default-storage-class STANDARD
gcloud storage buckets update "gs://$BUCKET" --project "$PROJECT" --update-labels "$LABELS" >/dev/null

exists bq --project_id "$PROJECT" show "$PROJECT:$DATASET" ||
  bq --project_id "$PROJECT" mk --location "$REGION" --label app:benchgrid --dataset "$PROJECT:$DATASET"

exists gc artifacts repositories describe "$REPO" --location "$REGION" ||
  gc artifacts repositories create "$REPO" --repository-format docker --location "$REGION" --labels "$LABELS"
echo "foundation ready: network $NETWORK, bucket gs://$BUCKET, dataset $PROJECT:$DATASET"
