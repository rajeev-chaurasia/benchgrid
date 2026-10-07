"""benchgrid-evaluate: measure how well the gate tells real regressions from
noise, on real rigs.

For every avbench profile it runs a number of null comparisons, where baseline
and candidate are the same binary with the same arguments, and a number of
injected ones, where the candidate runs with --slowdown at a known percentage.
A REGRESSION verdict on a null comparison is a false alarm; anything but
REGRESSION on an injected one is a miss. Every comparison, with its interval
and its runs, is written out, so every count in the summary can be recomputed.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict
from pathlib import Path

from .client import Client
from .gate import Config, build_spec, compare


def spec_template(profile: str, os_name: str, isolate: bool) -> dict:
    env: dict = {}
    if isolate:
        env["require_isolation"] = True
    return {
        "benchmark": f"avbench_{profile}",
        "revision": "0" * 40,
        "command": ["{binary}", "--profile", profile],
        "warmups": 2,
        "repetitions": 10,
        "timeout_seconds": 600,
        "requirements": {"os": os_name, "allow_emulated": True},
        "environment": env,
        "metrics": [{"name": "work_ns", "unit": "ns", "direction": "lower_is_better"}],
        "artifacts": {"binary_sha256": "0" * 64},
    }


def plan(profiles: list[str], nulls: int, injected: list[float], per_profile: int) -> list[tuple[str, float]]:
    """Each profile gets `nulls` null comparisons and `per_profile` injected
    ones, with the injected sizes cycling across profiles so every size is
    tried on many different kernels rather than all on one."""
    out = []
    k = 0
    for p in profiles:
        out += [(p, 0.0)] * nulls
        for _ in range(per_profile):
            out.append((p, injected[k % len(injected)]))
            k += 1
    return out


def summarize(results: list[dict]) -> dict:
    by_size: dict[str, Counter] = defaultdict(Counter)
    for r in results:
        key = "null" if r["injected"] == 0 else f"{r['injected']:g}%"
        by_size[key][r["verdict"]] += 1
    nulls = by_size.get("null", Counter())
    injected = [r for r in results if r["injected"] > 0]
    caught = sum(1 for r in injected if r["verdict"] == "REGRESSION")
    return {
        "comparisons": len(results),
        "null_comparisons": sum(nulls.values()),
        "false_alarms": nulls["REGRESSION"],
        "injected_comparisons": len(injected),
        "injected_caught": caught,
        "by_size": {k: dict(v) for k, v in sorted(by_size.items())},
        "runs": sum(len(r["runs"]) for r in results),
        "reruns_for_noise": sum(r["reruns"] for r in results),
        "pairs_on_one_rig": sum(r["same_rig_pairs"] for r in results),
        "pairs": sum(r["pairs"] for r in results),
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="benchgrid-evaluate", description=__doc__.split("\n\n")[0])
    p.add_argument("--api", required=True)
    p.add_argument("--binary", required=True, help="avbench built for the rigs")
    p.add_argument("--revision", required=True)
    p.add_argument("--os", default="linux")
    p.add_argument("--isolate", action="store_true", help="require rigs that pin to isolated CPUs")
    p.add_argument("--nulls", type=int, default=6)
    p.add_argument("--injected", default="3,5,8,12")
    p.add_argument("--injected-per-profile", type=int, default=2)
    p.add_argument("--profiles", help="comma separated; default every profile avbench lists")
    p.add_argument("--concurrency", type=int, default=8)
    p.add_argument("--out", required=True)
    a = p.parse_args(argv)

    client = Client(a.api)
    sha = client.upload_blob(Path(a.binary).read_bytes())
    profiles = a.profiles.split(",") if a.profiles else _list_profiles(a.binary)
    work = plan(profiles, a.nulls, [float(x) for x in a.injected.split(",")], a.injected_per_profile)
    cfg = Config()

    def one(item: tuple[str, float]) -> dict:
        profile, pct = item
        t = spec_template(profile, a.os, a.isolate)
        cand_args = ["--slowdown", f"{pct:g}"] if pct else []
        r = compare(client, build_spec(t, a.revision, sha, []), build_spec(t, a.revision, sha, cand_args), cfg)
        return {"profile": profile, "injected": pct, **asdict(r)}

    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    results = []
    with ThreadPoolExecutor(a.concurrency) as pool, (out / "comparisons.jsonl").open("w") as f:
        for r in pool.map(one, work):
            results.append(r)
            f.write(json.dumps(r) + "\n")
            f.flush()
    summary = summarize(results)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


def _list_profiles(binary: str) -> list[str]:
    import subprocess

    try:
        return subprocess.run([binary, "--list"], capture_output=True, text=True, check=True).stdout.split()
    except OSError as e:
        raise SystemExit(f"cannot run {binary} here to list its profiles; pass --profiles: {e}")


if __name__ == "__main__":
    sys.exit(main())
