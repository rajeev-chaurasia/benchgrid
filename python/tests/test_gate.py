import itertools

import numpy as np

from benchgrid_tools.gate import Config, build_spec, compare


class FakeAPI:
    """Stands in for the benchgrid API: every run of a spec draws samples
    around a centre that depends on whether it carries --slowdown, and the
    first `noisy_first` runs are made very noisy to exercise reruns."""

    def __init__(self, slowdown=0.0, noisy_first=0, fail_on=None):
        self.rng = np.random.default_rng(0)
        self.slowdown = slowdown
        self.noisy_first = noisy_first
        self.fail_on = fail_on
        self.order = []
        self.specs = {}
        self.ids = itertools.count()

    def submit(self, spec, key, max_attempts=3):
        i = f"exp_{next(self.ids)}"
        self.specs[i] = spec
        self.order.append("candidate" if "--slowdown" in spec["command"] or spec.get("_cand") else "baseline")
        return i

    def wait(self, exp_id, timeout=0):
        n = int(exp_id.split("_")[1])
        state = "FAILED" if self.fail_on == n else "SUCCEEDED"
        return {"state": state, "status_reason": "", "attempt": 1, "rig_id": "rig-1"}

    def samples(self, exp_id, attempt, metric):
        n = int(exp_id.split("_")[1])
        cand = "--slowdown" in self.specs[exp_id]["command"]
        centre = 100 * (1 + self.slowdown / 100) if cand else 100.0
        noise = 0.5 if n < self.noisy_first else 0.005
        return list(centre * (1 + noise * self.rng.standard_normal(20)))


TEMPLATE = {"command": ["{binary}", "--profile", "p"], "artifacts": {}}


def specs(pct):
    base = build_spec(TEMPLATE, "a" * 40, "b" * 64, [])
    cand = build_spec(TEMPLATE, "a" * 40, "b" * 64, ["--slowdown", str(pct)] if pct else [])
    if not pct:
        cand["_cand"] = True
    return base, cand


def test_regression_found_in_minimum_pairs_with_alternating_order():
    api = FakeAPI(slowdown=10)
    r = compare(api, *specs(10), Config())
    assert r.verdict == "REGRESSION"
    assert r.pairs == 3
    assert api.order == ["baseline", "candidate", "candidate", "baseline", "baseline", "candidate"]
    assert r.same_rig_pairs == 3


def test_null_comparison_passes():
    r = compare(FakeAPI(), *specs(0), Config())
    assert r.verdict == "PASS"


def test_noisy_run_is_rerun_and_counted():
    api = FakeAPI(slowdown=10, noisy_first=1)
    r = compare(api, *specs(10), Config())
    assert r.reruns == 1
    assert r.runs[0].rerun_of_noise == 1
    assert r.verdict == "REGRESSION"


def test_failed_run_is_an_error_not_a_verdict():
    r = compare(FakeAPI(fail_on=1), *specs(5), Config())
    assert r.verdict == "ERROR"
    assert "FAILED" in r.error


def test_both_sides_share_an_affinity_key():
    api = FakeAPI()
    compare(api, *specs(0), Config(min_pairs=1, max_pairs=1))
    keys = {s["affinity"] for s in api.specs.values()}
    assert len(keys) == 1 and next(iter(keys)).startswith("cmp-")
