#!/usr/bin/env bash
# Boots a simulated diskless rig over PXE and fails unless it registers with
# the control plane and completes an experiment. Expects Postgres at
# BENCHGRID_DATABASE_URL and qemu-system-x86_64, iPXE ROMs, busybox and zstd.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d)
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$work/bin/" ./cmd/rigagent ./cmd/benchload
go build -o "$work/host/" ./cmd/benchgrid ./cmd/bgctl
# KREL picks the kernel to boot; the CI job installs Ubuntu's generic kernel
# for it, because the runner's own cloud kernel need not carry the e1000
# driver the simulated machine's network card needs.
deploy/pxe/build_initramfs.sh "$work/http" "$work/bin/rigagent" "${KREL:-$(uname -r)}"
mkdir -p "$work/tftp" && cp deploy/pxe/boot.ipxe "$work/tftp/"

"$work/host/benchgrid" -listen 127.0.0.1:18080 -artifacts "$work/store" -id pxe-cp > "$work/server.log" 2>&1 &
pids+=($!)
(cd "$work/http" && python3 -m http.server 8000 --bind 127.0.0.1 > "$work/http.log" 2>&1) &
pids+=($!)

accel=tcg
[ -w /dev/kvm ] && accel=kvm
qemu-system-x86_64 -machine accel=$accel -m 512 -smp 2 -nographic -no-reboot -boot n \
  -netdev "user,id=n0,tftp=$work/tftp,bootfile=boot.ipxe,hostfwd=tcp:127.0.0.1:19090-:9090" \
  -device e1000,netdev=n0 > "$work/serial.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 180); do
  curl -fsS http://127.0.0.1:18080/v1/rigs 2>/dev/null | grep -q '"pxe-rig-0"' && break
  sleep 2
done
if ! curl -fsS http://127.0.0.1:18080/v1/rigs | grep -q '"pxe-rig-0"'; then
  echo "the PXE rig never registered"; tail -40 "$work/serial.log"; exit 1
fi
echo "PXE rig registered:"
curl -fsS http://127.0.0.1:18080/v1/rigs | python3 -c "import json,sys; d=[r for r in json.load(sys.stdin) if r['id']=='pxe-rig-0'][0]['descriptor']; print(' ', d['hardware_class'], d['os'], d['arch'], d['kernel'], d['cpu_cores'], 'cores')"

cat > "$work/spec.json" <<SPEC
{"benchmark":"pxe_smoke","revision":"$(git rev-parse HEAD)","command":["{binary}","-rounds","20000"],
 "warmups":1,"repetitions":5,"timeout_seconds":300,"requirements":{"hardware_class":"pxe-qemu"},
 "environment":{},"metrics":[{"name":"iteration_latency","unit":"ns","direction":"lower_is_better"},
 {"name":"throughput","unit":"ops_per_s","direction":"higher_is_better"}],"artifacts":{"binary_sha256":"$(printf '0%.0s' $(seq 1 64))"}}
SPEC
BENCHGRID_URL=http://127.0.0.1:18080 "$work/host/bgctl" submit -spec "$work/spec.json" -binary "$work/bin/benchload" -wait -timeout 5m > "$work/result.json" || {
  cat "$work/result.json"; tail -40 "$work/serial.log"; exit 1; }
grep -q '"state": "SUCCEEDED"' "$work/result.json"
echo "pxe ok: a diskless rig booted over PXE, registered, and completed an experiment"
