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
         "--filter", "labels.app=benchgrid AND labels.role=rig AND (labels.tuning=tuned OR labels.tuning=default) AND status=RUNNING",
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
    """Rerunning with the same --tag resumes: every run's idempotency key is
    derived from the tag, so runs already made are returned by the control
    plane rather than made again."""
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


# ---- gpu --------------------------------------------------------------------------

GPU_SIZES = {"small": ["--batch", "4", "--size", "192"], "medium": ["--batch", "8", "--size", "256"], "large": ["--batch", "16", "--size", "320"]}


def gpu_spec(revision: str, args: list[str], reps: int) -> dict:
    """The gates are the hardware's own readings: the run waits for GPU
    utilization under 10% and temperature under 85 C, both from nvidia-smi,
    and needs a real NVIDIA GPU with a recent driver and enough memory."""
    return {
        "benchmark": "gpubench_perception",
        "revision": revision,
        "command": ["/usr/bin/python3", "{binary}", "--iters", "20"] + args,
        "warmups": 1,
        "repetitions": reps,
        "timeout_seconds": 1800,
        "requirements": {"os": "linux", "gpu_vendor": "nvidia", "driver": ">=550", "min_gpu_memory_bytes": 16 << 30},
        "environment": {"max_gpu_util": 0.10, "max_temp_c": 85, "max_clock_offset_ms": 5.0, "preflight_timeout_seconds": 120},
        "metrics": [
            {"name": "gpu_time_ns", "unit": "ns", "direction": "lower_is_better"},
            {"name": "frames_per_s", "unit": "ops_per_s", "direction": "higher_is_better"},
        ],
        "artifacts": {"binary_sha256": "0" * 64},
    }


def gpu(a: argparse.Namespace) -> int:
    from dataclasses import asdict

    from .gate import Config, compare

    client = Client(a.api)
    out = Path(a.out)
    (out / "store").mkdir(parents=True, exist_ok=True)
    sha = client.upload_blob(Path(a.binary).read_bytes())
    records = []
    with (out / "runs.jsonl").open("w") as f:
        for k in range(a.runs):
            for size, args in GPU_SIZES.items():
                spec = gpu_spec(a.revision, args, a.reps)
                spec["artifacts"]["binary_sha256"] = sha
                rec = fetch_run(client, client.submit(spec, f"gpu-{a.tag}-{size}-{k}", max_attempts=2), out / "store")
                rec["size"] = size
                records.append(rec)
                f.write(json.dumps(rec) + "\n")
                f.flush()
    # Two gate decisions on the GPU: the same model against itself, and the
    # model against one with an extra residual block per stage, a real change
    # to the work rather than injected spinning.
    cfg = Config(metric="gpu_time_ns", min_pairs=3, max_pairs=5, max_cv=0.05)
    base = gpu_spec(a.revision, GPU_SIZES["medium"], a.reps)
    base["artifacts"]["binary_sha256"] = sha
    deeper = json.loads(json.dumps(base))
    deeper["command"] += ["--depth", "3"]
    gates = []
    with (out / "comparisons.jsonl").open("w") as f:
        for label, cand in (("null", base), ("depth 2 to 3", deeper)):
            r = asdict(compare(client, base, cand, cfg))
            for run in r["runs"]:
                fetch_run(client, run["experiment"], out / "store")
            r["label"] = label
            gates.append(r)
            f.write(json.dumps(r) + "\n")
    summary = summarize_gpu(records, gates, out / "store")
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


def summarize_gpu(records: list[dict], gates: list[dict], store: Path) -> dict:
    sizes = {}
    rig = None
    temps, utils = [], []
    for size in GPU_SIZES:
        cvs, medians, fps, failed = [], [], [], 0
        for r in records:
            if r["size"] != size:
                continue
            if r["state"] != "SUCCEEDED":
                failed += 1
                continue
            d = store / "runs" / r["experiment"] / f"attempt-{r['attempt']}"
            cv, med = run_cv(d, "gpu_time_ns")
            _, fmed = run_cv(d, "frames_per_s")
            run = json.loads((d / "run.json").read_text())
            rig = rig or run["rig"]
            pre = run["environment"]["preflight_before"]
            if pre.get("temp_c") is not None:
                temps.append(pre["temp_c"])
            if pre.get("gpu_util") is not None:
                utils.append(pre["gpu_util"])
            cvs.append(cv)
            medians.append(med / 1e6 / 20)  # per forward pass, in ms
            fps.append(fmed)
        sizes[size] = {
            "runs": len(cvs) + failed, "not_succeeded": failed,
            "median_ms_per_pass": float(np.median(medians)) if medians else None,
            "median_frames_per_s": float(np.median(fps)) if fps else None,
            "median_cv": float(np.median(cvs)) if cvs else None,
        }
    return {
        "gpu": f"{rig['gpu_model']}, {rig['gpu_memory_bytes'] >> 20} MiB, driver {rig['driver_version']}" if rig else None,
        "emulated": rig["emulated"] if rig else None,
        "sizes": sizes,
        "preflight_temp_c": [min(temps), max(temps)] if temps else None,
        "preflight_gpu_util": [min(utils), max(utils)] if utils else None,
        "gates": [{"label": g["label"], "verdict": g["verdict"], "ratio": g["ratio"], "low": g["low"],
                   "high": g["high"], "pairs": g["pairs"]} for g in gates],
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
    if (root / "gate" / "summary.json").exists():
        bad += validate_gate(root / "gate")
    if (root / "gpu" / "summary.json").exists():
        d = root / "gpu"
        recs = [json.loads(l) for l in (d / "runs.jsonl").read_text().splitlines() if l]
        gates = [json.loads(l) for l in (d / "comparisons.jsonl").read_text().splitlines() if l]
        if summarize_gpu(recs, gates, d / "store") != json.loads((d / "summary.json").read_text()):
            bad.append("gpu summary does not match its runs")
    for b in bad:
        print("FAIL", b)
    print("ok" if not bad else f"{len(bad)} failures")
    return 1 if bad else 0


def validate_gate(d: Path) -> list[str]:
    """Recomputes every verdict from the published samples of the runs each
    comparison used, and the evaluation summary from the verdicts."""
    from .evaluate import summarize
    from .gate import Config
    from .stats import decide, ratio_interval

    bad = []
    cfg = Config()
    results = [json.loads(l) for l in (d / "comparisons.jsonl").read_text().splitlines() if l]
    for r in results:
        if r["verdict"] == "ERROR":
            continue
        pairs = []
        runs = r["runs"]
        for i in range(0, len(runs), 2):
            got = {}
            for run in runs[i:i + 2]:
                f = d / "store" / "runs" / run["experiment"] / f"attempt-{run['attempt']}" / "samples.jsonl"
                got[run["side"]] = [json.loads(l)["value"] for l in f.read_text().splitlines()
                                    if l and json.loads(l)["metric"] == cfg.metric and not json.loads(l)["warmup"]]
            pairs.append((got["baseline"], got["candidate"]))
        e = ratio_interval(pairs) if len(pairs) >= 1 else None
        if e is None or abs(e.ratio - r["ratio"]) > 1e-12 or decide(e, cfg.threshold).value != r["verdict"]:
            bad.append(f"comparison {r['profile']} {r['injected']}% does not recompute")
    if summarize(results) != json.loads((d / "summary.json").read_text()):
        bad.append("gate summary does not match its comparisons")
    return bad


def env(a: argparse.Namespace) -> int:
    """Records the fleet as it describes itself: what the rigs report through
    the API, and what GCP reports about the cluster. Nothing is typed in."""
    client = Client(a.api)
    rigs = json.loads(client._call("GET", "/v1/rigs"))
    bench = [r["descriptor"] for r in rigs if r["descriptor"]["hardware_class"].startswith("n2d")]
    gpus = [r["descriptor"] for r in rigs if r["descriptor"].get("gpu_vendor")]
    gke = subprocess.run(["gcloud", "container", "clusters", "describe", a.cluster, "--zone", a.zone,
                          "--project", a.project, "--format", "value(currentMasterVersion)"],
                         capture_output=True, text=True).stdout.strip()
    commit = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    rec = {
        "git_commit": commit,
        "zone": a.zone,
        "rig_machine": "n2d-standard-4, one thread per core",
        "cpu_model": bench[0]["cpu_model"] if bench else "",
        "kernel": bench[0]["kernel"] if bench else "",
        "tuned_rigs": sum(1 for d in bench if d["hardware_class"].endswith("isolated")),
        "default_rigs": sum(1 for d in bench if d["hardware_class"].endswith("default")),
        "gpu": f"{gpus[0]['gpu_model']} driver {gpus[0]['driver_version']}" if gpus else "",
        "control_plane": f"GKE {gke}, two replicas, Postgres 16 in cluster",
        "notes": a.note or [],
    }
    Path(a.out).mkdir(parents=True, exist_ok=True)
    (Path(a.out) / "env.json").write_text(json.dumps(rec, indent=2) + "\n")
    print(json.dumps(rec, indent=2))
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="benchgrid-study", description=__doc__.split("\n\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    for name in ("isolation", "scale", "gpu"):
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
    e = sub.add_parser("env")
    e.add_argument("--api", required=True)
    e.add_argument("--project", required=True)
    e.add_argument("--zone", default="us-west1-b")
    e.add_argument("--cluster", default="benchgrid")
    e.add_argument("--note", action="append")
    e.add_argument("--out", required=True)
    a = p.parse_args(argv)
    return {"isolation": isolation, "scale": scale, "gpu": gpu, "validate": validate, "env": env}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
