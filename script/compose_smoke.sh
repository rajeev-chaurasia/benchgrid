#!/usr/bin/env bash
# Brings the compose stack up, runs one experiment on each of its three
# Linux rigs through both replicas, and fails unless all three succeed and
# their artifacts are sealed in the shared store. Tears everything down on
# exit, whatever happened.
set -euo pipefail
cd "$(dirname "$0")/.."

compose=(docker compose -f deploy/compose/docker-compose.yml)
cleanup() { "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

make linux build >/dev/null
docker build -q -t benchgrid:local . >/dev/null
"${compose[@]}" up -d >/dev/null

url=http://127.0.0.1:18080
for _ in $(seq 1 60); do
  ready=$(curl -fsS "$url/v1/rigs" 2>/dev/null | grep -o '"agent_state":"READY"' | wc -l | tr -d ' ' || true)
  [ "${ready:-0}" = 3 ] && break
  sleep 1
done
[ "${ready:-0}" = 3 ] || { echo "only ${ready:-0} of 3 rigs registered"; "${compose[@]}" logs --tail 20; exit 1; }

spec=$(mktemp)
sed -e 's/"requirements": {"allow_emulated": true}/"requirements": {"os": "linux", "allow_emulated": true}/' \
    -e 's/"environment": {[^}]*}/"environment": {}/' examples/cpu_hash.json > "$spec"

pids=()
for i in 1 2 3; do
  BENCHGRID_URL=$url ./bin/bgctl submit -spec "$spec" -binary bin/linux/benchload -key "smoke-$i-$$" -wait -timeout 3m > "$spec.$i" &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p"; done

for i in 1 2 3; do
  grep -q '"state": "SUCCEEDED"' "$spec.$i" || { cat "$spec.$i"; exit 1; }
done
# The benchgrid image has no shell, so the shared store is inspected from the
# postgres image, which is already part of the stack.
sealed=$(docker run --rm -v benchgrid_store:/store postgres:16 sh -c 'ls /store/runs/*/attempt-*/manifest.json 2>/dev/null | wc -l' | tr -d ' ')
[ "$sealed" -ge 3 ] || { echo "only $sealed sealed attempts in the shared store"; exit 1; }
echo "compose smoke ok: 3 experiments succeeded on 3 Linux rigs, $sealed sealed attempts in the shared store"
