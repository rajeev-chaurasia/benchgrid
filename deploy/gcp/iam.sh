#!/usr/bin/env bash
# Three service accounts, each with only what its job needs. Grants are on
# the benchgrid bucket and registry where GCP allows that; logging, metrics
# and the GKE node role exist only at project level.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh

sa() { echo "$1@$PROJECT.iam.gserviceaccount.com"; }
make_sa() {
  gc iam service-accounts describe "$(sa "$1")" >/dev/null 2>&1 ||
    gc iam service-accounts create "$1" --display-name "$2"
}
bucket_role() { gcloud storage buckets add-iam-policy-binding "gs://$1" --project "$PROJECT" --member "serviceAccount:$(sa "$2")" --role "$3" >/dev/null; }
repo_role() { gc artifacts repositories add-iam-policy-binding "$REPO" --location "$REGION" --member "serviceAccount:$(sa "$1")" --role "$2" >/dev/null; }
project_role() { gc projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$(sa "$1")" --role "$2" --condition None >/dev/null; }

make_sa benchgrid-build "benchgrid Cloud Build"
make_sa benchgrid-rig "benchgrid bench nodes"
make_sa benchgrid-node "benchgrid GKE nodes"
# A newly created account can take a few seconds to be visible to IAM.
sleep 10

bucket_role "$BUCKET" benchgrid-build roles/storage.objectAdmin
gcloud storage buckets describe "gs://${PROJECT}_cloudbuild" --project "$PROJECT" >/dev/null 2>&1 ||
  gcloud storage buckets create "gs://${PROJECT}_cloudbuild" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access
bucket_role "${PROJECT}_cloudbuild" benchgrid-build roles/storage.objectAdmin
repo_role benchgrid-build roles/artifactregistry.writer
project_role benchgrid-build roles/logging.logWriter

bucket_role "$BUCKET" benchgrid-rig roles/storage.objectViewer
project_role benchgrid-rig roles/logging.logWriter
project_role benchgrid-rig roles/monitoring.metricWriter

bucket_role "$BUCKET" benchgrid-node roles/storage.objectAdmin
repo_role benchgrid-node roles/artifactregistry.reader
project_role benchgrid-node roles/container.defaultNodeServiceAccount
echo "service accounts ready"
