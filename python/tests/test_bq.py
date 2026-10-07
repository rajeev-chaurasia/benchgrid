import json
import shutil
from pathlib import Path

import pytest

from benchgrid_tools.bq import Corrupt, iter_local, rows

ROOT = Path(__file__).resolve().parents[2]


def published_store():
    stores = sorted(ROOT.glob("evidence/results/*/chaos/store"))
    if not stores:
        pytest.skip("no published chaos store")
    return stores[-1]


def test_rows_from_published_runs():
    got = list(iter_local(published_store()))
    assert got and not [g for g in got if isinstance(g[2], Exception)]
    run, samples = rows(got[0][2], "2026-10-07T00:00:00+00:00")
    assert run["run_id"] == got[0][0] and run["attempt"] == got[0][1]
    assert run["finished"].endswith("+00:00")
    assert all(s["run_id"] == run["run_id"] and s["finished"] == run["finished"] for s in samples)
    measured = [s for s in samples if not s["warmup"] and s["metric"] == "iteration_latency"]
    summary = json.loads(run["summary"])
    assert summary["iteration_latency"]["n"] == len(measured)


def test_tampered_attempt_is_reported_not_loaded(tmp_path):
    src = published_store()
    one = sorted(src.glob("runs/*/attempt-*"))[0]
    dst = tmp_path / "runs" / one.parent.name / one.name
    shutil.copytree(one, dst)
    run = dst / "run.json"
    run.write_bytes(run.read_bytes().replace(b'"attempt"', b'"attempt" ', 1))
    got = list(iter_local(tmp_path))
    assert len(got) == 1 and isinstance(got[0][2], Corrupt)
