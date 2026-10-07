from benchgrid_tools.evaluate import plan, summarize


def test_plan_cycles_sizes_across_profiles():
    p = plan(["a", "b", "c"], nulls=2, injected=[3, 12], per_profile=2)
    assert len(p) == 12
    assert [x for x in p if x[1] == 0] == [("a", 0.0)] * 2 + [("b", 0.0)] * 2 + [("c", 0.0)] * 2
    sizes = [x[1] for x in p if x[1]]
    assert sizes.count(3) == 3 and sizes.count(12) == 3


def test_summarize_counts_false_alarms_and_catches():
    res = [
        {"injected": 0, "verdict": "PASS", "runs": [1, 2], "reruns": 0, "same_rig_pairs": 1, "pairs": 1},
        {"injected": 0, "verdict": "REGRESSION", "runs": [1, 2], "reruns": 1, "same_rig_pairs": 1, "pairs": 1},
        {"injected": 5, "verdict": "REGRESSION", "runs": [1, 2], "reruns": 0, "same_rig_pairs": 0, "pairs": 1},
        {"injected": 3, "verdict": "INCONCLUSIVE", "runs": [1, 2], "reruns": 0, "same_rig_pairs": 1, "pairs": 1},
    ]
    s = summarize(res)
    assert s["false_alarms"] == 1 and s["injected_caught"] == 1 and s["injected_comparisons"] == 2
    assert s["by_size"]["3%"] == {"INCONCLUSIVE": 1}
