"""benchgrid-study: the measurements made on the GCP fleet.

isolation  For every avbench profile, runs on tuned nodes (benchmark pinned
           to a kernel-isolated core in its own cgroup) and default nodes
           (same machine type, nothing tuned), with a noise source on the
           system core switched off and then on. Conditions alternate per
           profile rather than running in blocks, so drift over time cannot
           read as a difference between them.

scale      Replays many short jobs across the whole fleet and records, for
           every one, how it ended, how long it waited, and how long it ran,
           from the control plane's own timestamps.

Both write every run they cause to a results directory, with each run's
sealed files copied out of the store, so every published number can be
recomputed from raw samples by `benchgrid-study validate`.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from pathlib import Path

import numpy as np

from .client import Client

TUNED, DEFAULT = "n2d-isolated", "n2d-default"


def avbench_spec(profile: str, revision: str, cls: str, warmups: int, reps: int, isolate: bool) -> dict:
    env: dict = {"max_clock_offset_ms": 5.0}
    if isolate:
        env["require_isolation"] = True
    return {
        "benchmark": f"avbench_{profile}",
        "revision": revision,
        "command": ["{binary}", "--profile", profile],
        "warmups": warmups,
        "repetitions": reps,
        "timeout_seconds": 600,
        "requirements": {"os": "linux", "hardware_class": cls},
        "environment": env,
        "metrics": [
            {"name": "work_ns", "unit": "ns", "direction": "lower_is_better"},
            {"name": "iteration_latency", "unit": "ns", "direction": "lower_is_better"},
        ],
        "artifacts": {"binary_sha256": "0" * 64},
    }


# ---- fleet control ------------------------------------------------------------


def rig_instances(project: str) -> list[tuple[str, str]]:
    out = subprocess.run(
        ["gcloud", "compute", "instances", "list", "--project", project,
         "--filter", "labels.app=benchgrid AND labels.role=rig AND status=RUNNING",
         "--format", "value(name,zone.basename())"],
        capture_output=True, text=True, check=True,
    ).stdout
    return [tuple(line.split()) for line in out.splitlines() if line.strip()]


def set_noise(project: str, value: str) -> None:
    """Switches the noise source on every rig at once, and waits long enough
    for every node's watcher to notice."""
    def one(inst):
        name, zone = inst
        subprocess.run(["gcloud", "compute", "instances", "add-metadata", name, "--zone", zone,
                        "--project", project, "--metadata", f"benchgrid-noise={value}", "--quiet"],
                       capture_output=True, check=True)
    with ThreadPoolExecutor(16) as pool:
        list(pool.map(one, rig_instances(project)))
    time.sleep(5)


# ---- collecting runs ------------------------------------------------------------


def fetch_run(client: Client, exp_id: str, dest: Path) -> dict:
    """Waits for an experiment and copies its final attempt's sealed files."""
    e = client.wait(exp_id, timeout=1800)
    rec = {"experiment": exp_id, "state": e["state"], "reason": e["status_reason"],
           "attempt": e["attempt"], "rig": e["attempts"][-1]["rig_id"] if e.get("attempts") else None,
           "submitted_at": e["submitted_at"],
           "leased_at": e["attempts"][-1]["leased_at"] if e.get("attempts") else None,
           "finished_at": e["attempts"][-1]["finished_at"] if e.get("attempts") else None}
    if e["attempt"] >= 1 and dest is not None:
        d = dest / "runs" / exp_id / f"attempt-{e['attempt']}"
        d.mkdir(parents=True, exist_ok=True)
        try:
            manifest = json.loads(client._call("GET", f"/v1/artifacts/runs/{exp_id}/attempt-{e['attempt']}/manifest.json"))
            for f in manifest["files"] + [{"path": "manifest.json"}]:
                (d / f["path"]).write_bytes(client._call("GET", f"/v1/artifacts/runs/{exp_id}/attempt-{e['attempt']}/{f['path']}"))
        except Exception as err:  # noqa: BLE001 - recorded, and caught by validation
            rec["fetch_error"] = str(err)
    return rec


def run_cv(run_dir: Path, metric: str) -> tuple[float | None, float | None]:
    if not (run_dir / "samples.jsonl").exists():
        return None, None
    vals = [json.loads(l)["value"] for l in (run_dir / "samples.jsonl").read_text().splitlines()
            if l and json.loads(l)["metric"] == metric and not json.loads(l)["warmup"]]
    if len(vals) < 2:
        return None, None
    a = np.asarray(vals, dtype=float)
    return float(a.std(ddof=1) / a.mean()), float(np.median(a))


# ---- isolation ------------------------------------------------------------------


def isolation(a: argparse.Namespace) -> int:
    client = Client(a.api)
    out = Path(a.out)
    (out / "store").mkdir(parents=True, exist_ok=True)
    sha = client.upload_blob(Path(a.binary).read_bytes())
    profiles = a.profiles.split(",")
    records = []
    with (out / "runs.jsonl").open("w") as f:
        for p_i, profile in enumerate(profiles):
            # Alternate which condition goes first, profile by profile.
            conditions = ["quiet", "noise"] if p_i % 2 == 0 else ["noise", "quiet"]
            for cond in conditions:
                set_noise(a.project, "on" if cond == "noise" else "off")
                ids = []
                for k in range(a.runs):
                    for cls in (TUNED, DEFAULT):
                        spec = avbench_spec(profile, a.revision, cls, a.warmups, a.reps, cls == TUNED)
                        spec["artifacts"]["binary_sha256"] = sha
                        key = f"iso-{a.tag}-{profile}-{cond}-{cls}-{k}"
                        ids.append((cls, client.submit(spec, key, max_attempts=1)))
                with ThreadPoolExecutor(len(ids)) as pool:
                    got = list(pool.map(lambda x: fetch_run(client, x[1], out / "store"), ids))
                for (cls, _), rec in zip(ids, got):
                    rec.update({"profile": profile, "class": cls, "condition": cond})
                    records.append(rec)
                    f.write(json.dumps(rec) + "\n")
                    f.flush()
            print(f"{profile} done", file=sys.stderr)
    set_noise(a.project, "off")
    summary = summarize_isolation(records, out / "store", a.metric)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


def summarize_isolation(records: list[dict], store: Path, metric: str) -> dict:
    """Per class and condition: the median over profiles of each profile's
    median run CV, the per-profile values behind it, and how many runs did not
    succeed. A run that did not succeed contributes no CV and is counted."""
    cells: dict[str, dict] = {}
    for cls in (TUNED, DEFAULT):
        for cond in ("quiet", "noise"):
            per_profile: dict[str, list[float]] = {}
            failed = 0
            for r in records:
                if r["class"] != cls or r["condition"] != cond:
                    continue
                if r["state"] != "SUCCEEDED":
                    failed += 1
                    continue
                cv, _ = run_cv(store / "runs" / r["experiment"] / f"attempt-{r['attempt']}", metric)
                if cv is not None:
                    per_profile.setdefault(r["profile"], []).append(cv)
            medians = {p: float(np.median(v)) for p, v in sorted(per_profile.items())}
            cells[f"{cls}/{cond}"] = {
                "runs": sum(len(v) for v in per_profile.values()) + failed,
                "not_succeeded": failed,
                "profiles": len(medians),
                "median_cv": float(np.median(list(medians.values()))) if medians else None,
                "per_profile_median_cv": medians,
            }
    return {"metric": metric, "cells": cells}


# ---- scale ------------------------------------------------------------------------


def scale(a: argparse.Namespace) -> int:
    client = Client(a.api, timeout=60)
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    sha = client.upload_blob(Path(a.binary).read_bytes())
    profiles = a.profiles.split(",")

    def submit(i: int) -> str:
        profile = profiles[i % len(profiles)]
        cls = (TUNED, DEFAULT)[i % 2]
        spec = avbench_spec(profile, a.revision, cls, 0, a.reps, cls == TUNED)
        spec["artifacts"]["binary_sha256"] = sha
        return client.submit(spec, f"scale-{a.tag}-{i}")

    started = time.time()
    with ThreadPoolExecutor(16) as pool:
        ids = list(pool.map(submit, range(a.jobs)))
    print(f"submitted {len(ids)} in {time.time() - started:.0f}s", file=sys.stderr)
    with ThreadPoolExecutor(32) as pool, (out / "jobs.jsonl").open("w") as f:
        records = []
        for rec in pool.map(lambda e: fetch_run(client, e, None), ids):
            records.append(rec)
            f.write(json.dumps(rec) + "\n")
    summary = summarize_scale(records)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


def _t(s: str | None) -> float | None:
    return datetime.fromisoformat(s).timestamp() if s else None


def summarize_scale(records: list[dict]) -> dict:
    states: dict[str, int] = {}
    waits, runs = [], []
    first, last = None, None
    for r in records:
        states[r["state"]] = states.get(r["state"], 0) + 1
        sub, lease, fin = _t(r["submitted_at"]), _t(r["leased_at"]), _t(r["finished_at"])
        if sub and lease:
            waits.append(lease - sub)
        if lease and fin:
            runs.append(fin - lease)
        if sub:
            first = sub if first is None else min(first, sub)
        if fin:
            last = fin if last is None else max(last, fin)
    q = lambda xs, p: float(np.percentile(xs, p)) if xs else None  # noqa: E731
    n = len(records)
    return {
        "jobs": n,
        "states": dict(sorted(states.items())),
        "success_rate": states.get("SUCCEEDED", 0) / n if n else None,
        "wall_seconds": (last - first) if first and last else None,
        "jobs_per_minute": n / ((last - first) / 60) if first and last and last > first else None,
        "queue_wait_seconds": {"p50": q(waits, 50), "p95": q(waits, 95), "max": max(waits) if waits else None},
        "lease_to_finish_seconds": {"p50": q(runs, 50), "p95": q(runs, 95)},
    }


# ---- validation -------------------------------------------------------------------


def validate(a: argparse.Namespace) -> int:
    """Recomputes each study's summary from its raw files and fails on any
    difference, so a hand-edited number cannot survive CI."""
    root = Path(a.dir)
    bad = []
    if (root / "isolation" / "summary.json").exists():
        d = root / "isolation"
        recs = [json.loads(l) for l in (d / "runs.jsonl").read_text().splitlines() if l]
        pub = json.loads((d / "summary.json").read_text())
        if summarize_isolation(recs, d / "store", pub["metric"]) != pub:
            bad.append("isolation summary does not match its runs")
    if (root / "scale" / "summary.json").exists():
        d = root / "scale"
        recs = [json.loads(l) for l in (d / "jobs.jsonl").read_text().splitlines() if l]
        if summarize_scale(recs) != json.loads((d / "summary.json").read_text()):
            bad.append("scale summary does not match its jobs")
    for b in bad:
        print("FAIL", b)
    print("ok" if not bad else f"{len(bad)} failures")
    return 1 if bad else 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="benchgrid-study", description=__doc__.split("\n\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    for name in ("isolation", "scale"):
        s = sub.add_parser(name)
        s.add_argument("--api", required=True)
        s.add_argument("--project", required=True)
        s.add_argument("--binary", required=True)
        s.add_argument("--revision", required=True)
        s.add_argument("--profiles", required=True)
        s.add_argument("--tag", required=True, help="makes idempotency keys unique to this study")
        s.add_argument("--out", required=True)
        s.add_argument("--metric", default="work_ns")
        s.add_argument("--warmups", type=int, default=3)
        s.add_argument("--reps", type=int, default=20)
        s.add_argument("--runs", type=int, default=4, help="isolation: runs per profile, class and condition")
        s.add_argument("--jobs", type=int, default=12000, help="scale: how many jobs")
    v = sub.add_parser("validate")
    v.add_argument("dir")
    a = p.parse_args(argv)
    return {"isolation": isolation, "scale": scale, "validate": validate}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
