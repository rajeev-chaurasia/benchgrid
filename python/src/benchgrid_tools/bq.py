"""benchgrid-bq: load sealed runs into BigQuery.

Reads every sealed attempt from a run store, a directory or a gs:// prefix,
verifies each file against its manifest exactly as a consumer must, and
appends the attempts BigQuery does not already have to two tables: `runs`,
one row per attempt, and `samples`, one row per measured or warmup sample.
An attempt whose files do not match its manifest is reported and skipped,
never loaded.

Both tables are partitioned by the day the run finished and clustered by the
columns every performance query filters on first, so a query over one
benchmark on one hardware class scans that and little else.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator

RUNS_SCHEMA = [
    ("run_id", "STRING"), ("attempt", "INTEGER"), ("fence", "INTEGER"),
    ("status", "STRING"), ("status_reason", "STRING"),
    ("benchmark", "STRING"), ("spec_sha256", "STRING"), ("git_revision", "STRING"),
    ("binary_sha256", "STRING"),
    ("rig_id", "STRING"), ("hardware_class", "STRING"), ("os", "STRING"), ("arch", "STRING"),
    ("kernel", "STRING"), ("cpu_model", "STRING"), ("gpu_model", "STRING"),
    ("driver_version", "STRING"), ("emulated", "BOOLEAN"),
    ("lease_acquired", "TIMESTAMP"), ("started", "TIMESTAMP"), ("finished", "TIMESTAMP"),
    ("summary", "JSON"), ("host", "JSON"), ("exported_at", "TIMESTAMP"),
]

SAMPLES_SCHEMA = [
    ("run_id", "STRING"), ("attempt", "INTEGER"), ("benchmark", "STRING"),
    ("hardware_class", "STRING"), ("rig_id", "STRING"), ("git_revision", "STRING"),
    ("status", "STRING"), ("metric", "STRING"), ("unit", "STRING"),
    ("iteration", "INTEGER"), ("warmup", "BOOLEAN"), ("value", "FLOAT"),
    ("t_offset_ns", "INTEGER"), ("finished", "TIMESTAMP"),
]

CLUSTERING = {
    "runs": ["benchmark", "hardware_class", "status"],
    "samples": ["benchmark", "hardware_class", "metric"],
}


class Corrupt(Exception):
    pass


@dataclass
class Attempt:
    run_id: str
    attempt: int
    files: dict[str, bytes]


# ---- reading stores -----------------------------------------------------------


def _verify(manifest: dict, files: dict[str, bytes]) -> None:
    if manifest.get("schema_version") != "benchgrid.manifest/v1":
        raise Corrupt("manifest schema")
    for f in manifest["files"]:
        b = files.get(f["path"])
        if b is None:
            raise Corrupt(f"{f['path']} missing")
        if hashlib.sha256(b).hexdigest() != f["sha256"] or len(b) != f["size"]:
            raise Corrupt(f"{f['path']} does not match its manifest")


def iter_local(root: Path) -> Iterator[tuple[str, int, Attempt | Exception]]:
    for m in sorted(root.glob("runs/*/attempt-*/manifest.json")):
        run_id, attempt = m.parent.parent.name, int(m.parent.name.removeprefix("attempt-"))
        try:
            manifest = json.loads(m.read_bytes())
            files = {f["path"]: (m.parent / f["path"]).read_bytes() for f in manifest["files"] if (m.parent / f["path"]).exists()}
            _verify(manifest, files)
            yield run_id, attempt, Attempt(run_id, attempt, files)
        except (Corrupt, OSError, ValueError, KeyError) as e:
            yield run_id, attempt, e


def iter_gcs(url: str) -> Iterator[tuple[str, int, Attempt | Exception]]:
    from google.cloud import storage

    bucket_name, _, prefix = url.removeprefix("gs://").partition("/")
    bucket = storage.Client().bucket(bucket_name)
    base = (prefix.strip("/") + "/runs/") if prefix.strip("/") else "runs/"
    for blob in bucket.list_blobs(prefix=base):
        if not blob.name.endswith("/manifest.json"):
            continue
        parts = blob.name[len(base):].split("/")
        run_id, attempt = parts[0], int(parts[1].removeprefix("attempt-"))
        try:
            manifest = json.loads(blob.download_as_bytes())
            d = blob.name.rsplit("/", 1)[0]
            files = {f["path"]: bucket.blob(f"{d}/{f['path']}").download_as_bytes() for f in manifest["files"]}
            _verify(manifest, files)
            yield run_id, attempt, Attempt(run_id, attempt, files)
        except Exception as e:  # noqa: BLE001 - every failure is reported per attempt
            yield run_id, attempt, e


# ---- rows -------------------------------------------------------------------


def _ts(s: str | None) -> str | None:
    if not s:
        return None
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc).isoformat()


def rows(a: Attempt, exported_at: str) -> tuple[dict, list[dict]]:
    run = json.loads(a.files["run.json"])
    rig, env, timing, spec = run["rig"], run["environment"], run["timing"], run["spec"]
    host = a.files.get("host.json")
    finished = _ts(timing["finished"])
    r = {
        "run_id": run["run_id"], "attempt": run["attempt"], "fence": run["fence"],
        "status": run["status"], "status_reason": run["status_reason"],
        "benchmark": spec["benchmark"], "spec_sha256": run["spec_sha256"],
        "git_revision": env["git_revision"], "binary_sha256": env["binary_sha256"],
        "rig_id": rig["rig_id"], "hardware_class": rig["hardware_class"], "os": rig["os"],
        "arch": rig["arch"], "kernel": rig["kernel"], "cpu_model": rig["cpu_model"],
        "gpu_model": rig["gpu_model"], "driver_version": rig["driver_version"],
        "emulated": rig["emulated"],
        "lease_acquired": _ts(timing["lease_acquired"]), "started": _ts(timing["started"]),
        "finished": finished,
        "summary": json.dumps(run["summary"]),
        "host": host.decode() if host else None,
        "exported_at": exported_at,
    }
    samples = []
    for line in a.files["samples.jsonl"].splitlines():
        s = json.loads(line)
        samples.append({
            "run_id": run["run_id"], "attempt": run["attempt"], "benchmark": spec["benchmark"],
            "hardware_class": rig["hardware_class"], "rig_id": rig["rig_id"],
            "git_revision": env["git_revision"], "status": run["status"],
            "metric": s["metric"], "unit": s["unit"], "iteration": s["iteration"],
            "warmup": s["warmup"], "value": s["value"], "t_offset_ns": s["t_offset_ns"],
            "finished": finished,
        })
    return r, samples


# ---- BigQuery ---------------------------------------------------------------


def ensure_tables(client, dataset: str) -> None:
    from google.cloud import bigquery

    for name, schema in (("runs", RUNS_SCHEMA), ("samples", SAMPLES_SCHEMA)):
        t = bigquery.Table(f"{dataset}.{name}", schema=[bigquery.SchemaField(n, ty) for n, ty in schema])
        t.time_partitioning = bigquery.TimePartitioning(type_=bigquery.TimePartitioningType.DAY, field="finished")
        t.clustering_fields = CLUSTERING[name]
        t.labels = {"app": "benchgrid"}
        client.create_table(t, exists_ok=True)


def existing(client, dataset: str) -> set[tuple[str, int]]:
    q = f"SELECT DISTINCT run_id, attempt FROM `{dataset}.runs`"
    return {(r.run_id, r.attempt) for r in client.query(q).result()}


def load(client, table: str, data: list[dict]) -> None:
    from google.cloud import bigquery

    if not data:
        return
    # Load jobs, not streaming inserts: they are free, atomic per job, and
    # leave no streaming buffer to make a just-loaded run invisible to a query.
    job = client.load_table_from_json(
        data, table, job_config=bigquery.LoadJobConfig(write_disposition="WRITE_APPEND")
    )
    job.result()


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="benchgrid-bq", description=__doc__.split("\n\n")[0])
    p.add_argument("--store", required=True, help="a run store directory, or gs://bucket/prefix")
    p.add_argument("--dataset", required=True, help="project.dataset")
    p.add_argument("--dry-run", action="store_true", help="verify and count, load nothing")
    a = p.parse_args(argv)

    attempts = iter_gcs(a.store) if a.store.startswith("gs://") else iter_local(Path(a.store))
    client = None
    have: set[tuple[str, int]] = set()
    if not a.dry_run:
        from google.cloud import bigquery

        client = bigquery.Client(project=a.dataset.split(".")[0])
        ensure_tables(client, a.dataset)
        have = existing(client, a.dataset)

    now = datetime.now(timezone.utc).isoformat()
    run_rows, sample_rows, corrupt, skipped = [], [], [], 0
    for run_id, attempt, got in attempts:
        if isinstance(got, Exception):
            corrupt.append(f"{run_id}/attempt-{attempt}: {got}")
            continue
        if (run_id, attempt) in have:
            skipped += 1
            continue
        r, s = rows(got, now)
        run_rows.append(r)
        sample_rows.extend(s)
    if client is not None:
        load(client, f"{a.dataset}.runs", run_rows)
        load(client, f"{a.dataset}.samples", sample_rows)
    report = {"loaded_runs": len(run_rows), "loaded_samples": len(sample_rows),
              "already_present": skipped, "corrupt": corrupt, "dry_run": a.dry_run}
    print(json.dumps(report, indent=2))
    return 1 if corrupt else 0


if __name__ == "__main__":
    sys.exit(main())
