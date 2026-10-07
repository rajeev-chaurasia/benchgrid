import numpy as np
import pytest

from benchgrid_tools.stats import Estimate, Verdict, cv, decide, ratio_interval


def runs(rng, centre, n=20, noise=0.01):
    return list(centre * (1 + noise * rng.standard_normal(n)))


def test_identical_builds_pass():
    rng = np.random.default_rng(1)
    pairs = [(runs(rng, 100), runs(rng, 100)) for _ in range(4)]
    e = ratio_interval(pairs)
    assert e.low < 1.0 < e.high
    assert decide(e, 0.02) == Verdict.PASS


def test_clear_regression_is_caught():
    rng = np.random.default_rng(2)
    pairs = [(runs(rng, 100), runs(rng, 108)) for _ in range(4)]
    e = ratio_interval(pairs)
    assert 1.06 < e.ratio < 1.10
    assert decide(e, 0.02) == Verdict.REGRESSION


def test_improvement_is_not_a_regression():
    rng = np.random.default_rng(3)
    pairs = [(runs(rng, 100), runs(rng, 90)) for _ in range(4)]
    assert decide(ratio_interval(pairs), 0.02) == Verdict.IMPROVEMENT


def test_a_real_but_tiny_change_is_not_blocked():
    # Statistically clear, practically irrelevant: the threshold is the point.
    rng = np.random.default_rng(4)
    pairs = [(runs(rng, 100, 200, 0.001), runs(rng, 100.5, 200, 0.001)) for _ in range(6)]
    e = ratio_interval(pairs)
    assert e.low > 1.0
    assert decide(e, 0.02) == Verdict.PASS


def test_noise_that_hides_the_answer_is_inconclusive():
    rng = np.random.default_rng(5)
    pairs = [(runs(rng, 100, 10, 0.3), runs(rng, 103, 10, 0.3)) for _ in range(3)]
    assert decide(ratio_interval(pairs), 0.02) == Verdict.INCONCLUSIVE


def test_geometric_mean_cancels_symmetric_changes():
    pairs = [([100.0], [200.0]), ([100.0], [50.0])]
    assert ratio_interval(pairs, resamples=10).ratio == pytest.approx(1.0)


def test_interval_is_reproducible():
    rng = np.random.default_rng(6)
    pairs = [(runs(rng, 100), runs(rng, 101)) for _ in range(3)]
    assert ratio_interval(pairs) == ratio_interval(pairs)


def test_decide_boundaries():
    assert decide(Estimate(1.03, 1.001, 1.05, 3), 0.02) == Verdict.REGRESSION
    assert decide(Estimate(1.01, 1.001, 1.02, 3), 0.02) == Verdict.INCONCLUSIVE
    assert decide(Estimate(1.03, 0.99, 1.05, 3), 0.02) == Verdict.INCONCLUSIVE


def test_cv():
    assert cv([1.0]) == float("inf")
    assert cv([10.0, 10.0, 10.0]) == 0.0


def test_empty_input_is_refused():
    with pytest.raises(ValueError):
        ratio_interval([])
    with pytest.raises(ValueError):
        ratio_interval([([], [1.0])])
