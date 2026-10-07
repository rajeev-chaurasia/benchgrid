#!/usr/bin/env bash
# rigs.sh create COUNT tuned|default [FIRST_INDEX]
# rigs.sh gpu
# Bench nodes have four vCPUs with one thread per core, which is two physical
# cores: CPU 0 for the system and the agent, CPU 1 for benchmarks.
# With SMT on, an "isolated" vCPU would share a core with everything else.
set -euo pipefail
cd "$(dirname "$0")"
source env.sh

control_url() { kubectl get svc benchgrid -o jsonpath='http://{.status.loadBalancer.ingress[0].ip}:8080'; }
agent_url() { cat .agent-url 2>/dev/null || { echo "run ./images.sh first" >&2; exit 1; }; }

case "${1:-}" in
create)
  count=$2 mode=$3 first=${4:-0}
  family=${RIG_MACHINE%%-*}
  tuned=false class=$family-default
  [ "$mode" = tuned ] && tuned=true class=$family-isolated
  names=()
  for i in $(seq "$first" $((first + count - 1))); do names+=("benchgrid-rig-$mode-$i"); done
  gc compute instances create "${names[@]}" --zone "$RIG_ZONE" \
    --machine-type "$RIG_MACHINE" --threads-per-core 1 \
    --subnet "$SUBNET" --no-address \
    --image-family debian-12 --image-project debian-cloud --boot-disk-size 20GB --boot-disk-type pd-balanced \
    --service-account "$RIG_SA" --scopes cloud-platform \
    --labels "$LABELS,role=rig,tuning=$mode" \
    --metadata "benchgrid-tuned=$tuned,benchgrid-bench-cpus=1,benchgrid-class=$class,benchgrid-control=$(control_url),benchgrid-agent=$(agent_url)" \
    --metadata-from-file startup-script=rig-startup.sh
  ;;
gpu)
  # GPU=t4 is an n1-standard-4 with a T4 attached; GPU=l4 is a g2-standard-4,
  # which comes with its L4. Both use the deep learning image, which carries
  # the driver and PyTorch.
  case "${GPU:-t4}" in
    t4) machine=(--machine-type n1-standard-4 --accelerator type=nvidia-tesla-t4,count=1) class=gcp-t4 ;;
    l4) machine=(--machine-type g2-standard-4) class=gcp-l4 ;;
    *) echo "GPU must be t4 or l4" >&2; exit 2 ;;
  esac
  gc compute instances create benchgrid-rig-gpu-0 --zone "${GPU_ZONE:-$ZONE}" \
    "${machine[@]}" \
    --maintenance-policy TERMINATE \
    --subnet "$SUBNET" --no-address \
    --image-family pytorch-2-9-cu129-ubuntu-2204-nvidia-580 --image-project deeplearning-platform-release \
    --boot-disk-size 100GB --boot-disk-type pd-balanced \
    --service-account "$RIG_SA" --scopes cloud-platform \
    --labels "$LABELS,role=rig,tuning=gpu" \
    --metadata "install-nvidia-driver=True,benchgrid-tuned=false,benchgrid-class=$class,benchgrid-control=$(control_url),benchgrid-agent=$(agent_url)" \
    --metadata-from-file startup-script=rig-startup.sh
  ;;
*)
  echo "usage: rigs.sh create COUNT tuned|default [FIRST] | rigs.sh gpu" >&2
  exit 2
  ;;
esac
