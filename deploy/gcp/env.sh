# Shared settings for every script in this directory. Override any of them in
# the environment. Everything created is labelled app=benchgrid, which is what
# down.sh deletes by and what the project's budget alert is filtered on.
: "${PROJECT:?set PROJECT to the GCP project id}"
REGION=${REGION:-us-west1}
ZONE=${ZONE:-us-west1-b}
# Bench nodes may need a different zone from the cluster when a zone runs out
# of a machine type; the VPC spans the region, so they reach it either way.
RIG_ZONE=${RIG_ZONE:-$ZONE}
# n2d (AMD) rather than n2 (Intel) because n2 was out of capacity in every
# us-west1 zone when the evidence was produced. Either works: what matters is
# one thread per core, so an isolated CPU is a whole physical core.
RIG_MACHINE=${RIG_MACHINE:-n2d-standard-4}
NETWORK=${NETWORK:-benchgrid}
SUBNET=${SUBNET:-benchgrid-west}
CLUSTER=${CLUSTER:-benchgrid}
BUCKET=${BUCKET:-${PROJECT}-benchgrid}
DATASET=${DATASET:-benchgrid}
REPO=${REPO:-benchgrid}
LABELS=app=benchgrid
# Kubernetes credentials go here rather than into ~/.kube/config, so nothing
# these scripts do can change which cluster kubectl talks to elsewhere.
KUBECONFIG=${KUBECONFIG_BENCHGRID:-${TMPDIR:-/tmp}/benchgrid-kubeconfig}
export KUBECONFIG
# kubectl authenticates to GKE through gcloud's gke-gcloud-auth-plugin, which
# gcloud installs beside itself rather than on PATH.
PATH="$(dirname "$(readlink -f "$(command -v gcloud)")"):$PATH"
export PATH
BUILD_SA=benchgrid-build@$PROJECT.iam.gserviceaccount.com
RIG_SA=benchgrid-rig@$PROJECT.iam.gserviceaccount.com
NODE_SA=benchgrid-node@$PROJECT.iam.gserviceaccount.com
gc() { gcloud --project "$PROJECT" --quiet "$@"; }
