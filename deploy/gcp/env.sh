# Shared settings for every script in this directory. Override any of them in
# the environment. Everything created is labelled app=benchgrid, which is what
# down.sh deletes by and what the project's budget alert is filtered on.
: "${PROJECT:?set PROJECT to the GCP project id}"
REGION=${REGION:-us-west1}
ZONE=${ZONE:-us-west1-b}
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
gc() { gcloud --project "$PROJECT" --quiet "$@"; }
