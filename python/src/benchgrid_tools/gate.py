"""benchgrid-gate: decide whether a candidate build is slower than a baseline.

Runs baseline and candidate in pairs through benchgrid, alternating which goes
first so a drift over time cannot read as a difference between them, reruns
any run noisier than --max-cv, and adds pairs until the verdict is clear or
--max-pairs is reached. Exit codes: 0 pass or improvement, 1 regression,
3 inconclusive, 2 the comparison could not be made.
"""

from __future__ import annotations

import argparse
import copy
import json
import sys
import uuid
from dataclasses import asdict, dataclass, field
from pathlib import Path

from .client import Client
from .stats import Verdict, cv, decide, ratio_interval

EXIT = {Verdict.PASS: 0, Verdict.IMPROVEMENT: 0, Verdict.REGRESSION: 1, Verdict.INCONCLUSIVE: 3}


@dataclass
class Config:
    metric: str = "work_ns"
    threshold: float = 0.02
    min_pairs: int = 3
    max_pairs: int = 8
    max_cv: float = 0.05
    max_reruns: int = 2
    timeout: float = 1800


@dataclass
class Run:
    side: str
    experiment: str
    attempt: int
    rig: str | None
    cv: float
    rerun_of_noise: int


@dataclass
class Result:
    verdict: str
    ratio: float | None
    low: float | None
    high: float | None
    pairs: int
    reruns: int
    same_rig_pairs: int
    runs: list[Run] = field(default_factory=list)
    error: str = ""


class ComparisonError(RuntimeError):
    pass


def _run(client: Client, spec: dict, key: str, cfg: Config, side: str) -> tuple[list[float], Run]:
    """One side of one pair, rerun while its samples are noisier than the
    limit. A noisy run is replaced rather than kept, because its samples
    describe the machine at that moment more than the code, and the reruns
    are counted so the result says how often that happened."""
    reruns = 0
    while True:
        exp = client.submit(spec, f"{key}-{reruns}")
        e = client.wait(exp, cfg.timeout)
        if e["state"] != "SUCCEEDED":
            raise ComparisonError(f"{side} run {exp} ended {e['state']} {e['status_reason']}")
        samples = client.samples(exp, e["attempt"], cfg.metric)
        if not samples:
            raise ComparisonError(f"{side} run {exp} has no {cfg.metric} samples")
        noise = cv(samples)
        if noise <= cfg.max_cv or reruns >= cfg.max_reruns:
            return samples, Run(side, exp, e["attempt"], e.get("rig_id"), noise, reruns)
        reruns += 1


def compare(client: Client, baseline: dict, candidate: dict, cfg: Config) -> Result:
    cmp_id = uuid.uuid4().hex[:12]
    baseline, candidate = copy.deepcopy(baseline), copy.deepcopy(candidate)
    baseline["affinity"] = candidate["affinity"] = f"cmp-{cmp_id}"
    pairs: list[tuple[list[float], list[float]]] = []
    runs: list[Run] = []
    estimate = None
    verdict = Verdict.INCONCLUSIVE
    try:
        for i in range(cfg.max_pairs):
            order = [("baseline", baseline), ("candidate", candidate)]
            if i % 2:
                order.reverse()
            got = {}
            for side, spec in order:
                samples, run = _run(client, spec, f"{cmp_id}-{i}-{side}", cfg, side)
                got[side] = samples
                runs.append(run)
            pairs.append((got["baseline"], got["candidate"]))
            if len(pairs) >= cfg.min_pairs:
                estimate = ratio_interval(pairs)
                verdict = decide(estimate, cfg.threshold)
                if verdict != Verdict.INCONCLUSIVE:
                    break
    except ComparisonError as err:
        return Result("ERROR", None, None, None, len(pairs), 0, 0, runs, str(err))

    same_rig = 0
    for k in range(0, len(runs) - 1, 2):
        if runs[k].rig and runs[k].rig == runs[k + 1].rig:
            same_rig += 1
    return Result(
        verdict=verdict.value,
        ratio=estimate.ratio if estimate else None,
        low=estimate.low if estimate else None,
        high=estimate.high if estimate else None,
        pairs=len(pairs),
        reruns=sum(r.rerun_of_noise for r in runs),
        same_rig_pairs=same_rig,
        runs=runs,
    )


def build_spec(template: dict, revision: str, binary_sha: str, extra_args: list[str]) -> dict:
    spec = copy.deepcopy(template)
    spec["revision"] = revision
    spec["artifacts"] = {**spec.get("artifacts", {}), "binary_sha256": binary_sha}
    spec["command"] = list(spec["command"]) + list(extra_args)
    return spec


def summary_markdown(r: Result, cfg: Config) -> str:
    if r.verdict == "ERROR":
        return f"### benchgrid gate: error\n\n{r.error}\n"
    pct = lambda v: f"{(v - 1) * 100:+.2f}%"  # noqa: E731
    return (
        f"### benchgrid gate: {r.verdict}\n\n"
        f"| | |\n|---|---|\n"
        f"| {cfg.metric} change, candidate vs baseline | {pct(r.ratio)} |\n"
        f"| 95% interval | {pct(r.low)} to {pct(r.high)} |\n"
        f"| practical threshold | {cfg.threshold * 100:.1f}% |\n"
        f"| pairs | {r.pairs} ({r.same_rig_pairs} on one rig) |\n"
        f"| runs rerun for noise | {r.reruns} |\n"
    )


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="benchgrid-gate", description=__doc__.split("\n\n")[0])
    p.add_argument("--api", required=True)
    p.add_argument("--spec", required=True, help="spec template; revision, binary and extra args are filled in")
    p.add_argument("--baseline-binary", required=True)
    p.add_argument("--candidate-binary", required=True)
    p.add_argument("--baseline-rev", required=True)
    p.add_argument("--candidate-rev", required=True)
    p.add_argument("--baseline-arg", action="append", default=[])
    p.add_argument("--candidate-arg", action="append", default=[])
    p.add_argument("--metric", default=Config.metric)
    p.add_argument("--threshold", type=float, default=Config.threshold)
    p.add_argument("--min-pairs", type=int, default=Config.min_pairs)
    p.add_argument("--max-pairs", type=int, default=Config.max_pairs)
    p.add_argument("--max-cv", type=float, default=Config.max_cv)
    p.add_argument("--out", help="write the full result as JSON here")
    p.add_argument("--summary", help="append a markdown summary here, such as $GITHUB_STEP_SUMMARY")
    a = p.parse_args(argv)

    cfg = Config(a.metric, a.threshold, a.min_pairs, a.max_pairs, a.max_cv)
    client = Client(a.api)
    template = json.loads(Path(a.spec).read_text())
    base_sha = client.upload_blob(Path(a.baseline_binary).read_bytes())
    cand_sha = client.upload_blob(Path(a.candidate_binary).read_bytes())
    result = compare(
        client,
        build_spec(template, a.baseline_rev, base_sha, a.baseline_arg),
        build_spec(template, a.candidate_rev, cand_sha, a.candidate_arg),
        cfg,
    )
    if a.out:
        Path(a.out).write_text(json.dumps(asdict(result), indent=2) + "\n")
    md = summary_markdown(result, cfg)
    print(md)
    if a.summary:
        with open(a.summary, "a") as f:
            f.write(md)
    if result.verdict == "ERROR":
        return 2
    return EXIT[Verdict(result.verdict)]


if __name__ == "__main__":
    sys.exit(main())
