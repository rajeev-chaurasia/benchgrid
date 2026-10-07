"""Paired comparison statistics for the CI gate.

A comparison is a set of pairs. Each pair is one baseline run and one
candidate run, measured back to back on, ideally, the same rig, and each run
is a list of measured samples. The quantity estimated is the ratio of
candidate to baseline median, summarised across pairs as a geometric mean,
so a candidate twice as fast and one twice as slow cancel rather than
averaging to 1.25.

The interval is a two level bootstrap: pairs are resampled, and within each
resampled pair, both runs' samples are resampled. Resampling only samples
would treat thirty iterations of one run as thirty independent measurements
of the code, when they share one machine state; resampling only pairs would
ignore how noisy each run was. Both levels together are what the variance
actually is.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

import numpy as np


class Verdict(str, Enum):
    PASS = "PASS"
    IMPROVEMENT = "IMPROVEMENT"
    REGRESSION = "REGRESSION"
    INCONCLUSIVE = "INCONCLUSIVE"


@dataclass(frozen=True)
class Estimate:
    ratio: float
    low: float
    high: float
    pairs: int


def _medians(rng: np.random.Generator, runs: list[np.ndarray], resample: bool) -> np.ndarray:
    out = np.empty(len(runs))
    for i, r in enumerate(runs):
        s = r[rng.integers(0, len(r), len(r))] if resample else r
        out[i] = np.median(s)
    return out


def ratio_interval(
    pairs: list[tuple[list[float], list[float]]],
    confidence: float = 0.95,
    resamples: int = 2000,
    seed: int = 0,
) -> Estimate:
    """Geometric mean of candidate/baseline median ratios, with a two level
    bootstrap interval. The seed is fixed so a verdict can be reproduced from
    the same samples."""
    if not pairs:
        raise ValueError("no pairs")
    base = [np.asarray(b, dtype=float) for b, _ in pairs]
    cand = [np.asarray(c, dtype=float) for _, c in pairs]
    if any(len(r) == 0 for r in base + cand):
        raise ValueError("a run has no samples")

    def statistic(bm: np.ndarray, cm: np.ndarray) -> float:
        return float(np.exp(np.mean(np.log(cm / bm))))

    rng = np.random.default_rng(seed)
    point = statistic(_medians(rng, base, False), _medians(rng, cand, False))
    n = len(pairs)
    boots = np.empty(resamples)
    for k in range(resamples):
        idx = rng.integers(0, n, n)
        bm = _medians(rng, [base[i] for i in idx], True)
        cm = _medians(rng, [cand[i] for i in idx], True)
        boots[k] = statistic(bm, cm)
    alpha = (1 - confidence) / 2
    low, high = np.quantile(boots, [alpha, 1 - alpha])
    return Estimate(ratio=point, low=float(low), high=float(high), pairs=n)


def decide(e: Estimate, threshold: float) -> Verdict:
    """A regression must be both statistically clear, its whole interval above
    no change, and practically meaningful, its estimate at least threshold
    slower. Passing needs the interval to rule out a regression of threshold
    or more. Anything between is inconclusive, which is a request for more
    data, never a quiet pass."""
    if e.low > 1.0 and e.ratio >= 1.0 + threshold:
        return Verdict.REGRESSION
    if e.high < 1.0 - threshold:
        return Verdict.IMPROVEMENT
    if e.high < 1.0 + threshold:
        return Verdict.PASS
    return Verdict.INCONCLUSIVE


def cv(samples: list[float]) -> float:
    a = np.asarray(samples, dtype=float)
    if len(a) < 2 or a.mean() == 0:
        return float("inf")
    return float(a.std(ddof=1) / a.mean())
