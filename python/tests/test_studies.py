import json

from benchgrid_tools.studies import DEFAULT, TUNED, summarize_isolation, summarize_scale


def write_run(store, exp, values):
    d = store / "runs" / exp / "attempt-1"
    d.mkdir(parents=True)
    lines = [json.dumps({"metric": "work_ns", "iteration": i, "warmup": False, "value": v, "unit": "ns", "t_offset_ns": 0})
             for i, v in enumerate(values)]
    (d / "samples.jsonl").write_text("\n".join(lines) + "\n")


def test_isolation_summary_counts_failures_and_takes_medians(tmp_path):
    recs = []
    for i, (cls, cond, vals) in enumerate([
        (TUNED, "quiet", [100, 101, 99]),
        (TUNED, "quiet", [100, 100, 100]),
        (DEFAULT, "noise", [100, 150, 80]),
    ]):
        write_run(tmp_path, f"e{i}", vals)
        recs.append({"experiment": f"e{i}", "attempt": 1, "state": "SUCCEEDED", "profile": "p", "class": cls, "condition": cond})
    recs.append({"experiment": "ex", "attempt": 1, "state": "INVALID", "profile": "p", "class": DEFAULT, "condition": "noise"})
    s = summarize_isolation(recs, tmp_path, "work_ns")
    tq = s["cells"][f"{TUNED}/quiet"]
    assert tq["runs"] == 2 and tq["not_succeeded"] == 0
    assert abs(tq["median_cv"] - 0.005) < 1e-9  # median of 0.01 and 0.0
    dn = s["cells"][f"{DEFAULT}/noise"]
    assert dn["runs"] == 2 and dn["not_succeeded"] == 1
    assert s["cells"][f"{TUNED}/noise"]["median_cv"] is None


def test_scale_summary():
    recs = [
        {"state": "SUCCEEDED", "submitted_at": "2026-10-07T00:00:00+00:00", "leased_at": "2026-10-07T00:00:02+00:00", "finished_at": "2026-10-07T00:00:05+00:00"},
        {"state": "FAILED", "submitted_at": "2026-10-07T00:00:00+00:00", "leased_at": "2026-10-07T00:00:04+00:00", "finished_at": "2026-10-07T00:01:00+00:00"},
    ]
    s = summarize_scale(recs)
    assert s["jobs"] == 2 and s["success_rate"] == 0.5 and s["states"] == {"FAILED": 1, "SUCCEEDED": 1}
    assert s["wall_seconds"] == 60 and s["queue_wait_seconds"]["max"] == 4
