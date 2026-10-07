#!/usr/bin/env bash
# Builds this commit on Cloud Build and records where the results went: the
# control plane image in Artifact Registry, and the rig agent, avbench and
# the rest in the bucket under bin/<commit>/.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh
TAG=$(git rev-parse --short=12 HEAD)
[ -z "$(git status --porcelain)" ] || { echo "commit first: the build is labelled with the commit, so it must be the commit" >&2; exit 1; }
IMAGE="$REGION-docker.pkg.dev/$PROJECT/$REPO/benchgrid"
gc services enable cloudbuild.googleapis.com
(cd ../.. && gcloud builds submit --project "$PROJECT" --region "$REGION" --config deploy/gcp/cloudbuild.yaml \
  --substitutions "_BUCKET=$BUCKET,_REPO_IMAGE=$IMAGE,_TAG=$TAG" .)
echo "gs://$BUCKET/bin/$TAG/rigagent" > .agent-url
echo "$IMAGE:$TAG" > .image
echo "built $TAG"
