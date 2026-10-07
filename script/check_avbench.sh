#!/usr/bin/env bash
# Builds avbench and fails unless every profile prints the same checksum on
# two runs, and the same checksum again under an injected slowdown. A
# profile that fails either cannot be used to measure a regression: its
# output, and so its work, would differ between baseline and candidate.
set -euo pipefail
cd "$(dirname "$0")/.."
build=${1:-build/avbench}
cmake -S workloads/avbench -B "$build" -DCMAKE_BUILD_TYPE=Release >/dev/null
cmake --build "$build" >/dev/null
bin=$build/avbench
n=0
for p in $("$bin" --list); do
  a=$("$bin" --profile "$p" --reps 1 | awk '/CHECKSUM/{print $2}')
  b=$("$bin" --profile "$p" --reps 1 | awk '/CHECKSUM/{print $2}')
  c=$("$bin" --profile "$p" --reps 1 --slowdown 10 | awk '/CHECKSUM/{print $2}')
  if [ -z "$a" ] || [ "$a" != "$b" ] || [ "$a" != "$c" ]; then
    echo "$p is not deterministic: $a $b $c"
    exit 1
  fi
  n=$((n + 1))
done
[ "$n" -eq 30 ] || { echo "expected 30 profiles, found $n"; exit 1; }
echo "avbench ok: $n profiles deterministic, under slowdown too"
