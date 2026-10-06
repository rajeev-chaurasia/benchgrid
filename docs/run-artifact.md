# Run artifact contract, v1

This is the boundary between benchgrid and TraceLab. benchgrid writes it and
TraceLab reads it, and nothing else passes between them. Everything below is
normative. Where this file and the code disagree, the code is wrong.

The summary statistics in a run are a checked claim, not the source of truth.
TraceLab recomputes every one of them from the raw samples and rejects the run
if they disagree, so the rules for computing them are spelled out exactly.

## Layout

```
<store>/runs/<run_id>/attempt-<attempt>/
    run.json
    samples.jsonl
    manifest.json
```

`run_id` is the experiment id, so every attempt at one experiment lands under
one directory. `attempt` is a counter on the experiment row, incremented in the
same transaction that claims the experiment for scheduling, and written in
decimal with no padding. It starts at 1. Two attempts at one experiment can
never share a number, because the claim and the increment are one statement.

The lease fence is not the attempt identity and must not be used as one. The
fence is counted per rig, so a retry that lands on a different rig can carry a
smaller fence than the attempt it replaced, and two attempts on two rigs can
carry the same fence. The fence is recorded in `run.json` for audit and does
its real job at the rig, nowhere else.

A consumer ingests only the sealed attempt with the highest `attempt` for a
`run_id`, and checks that `run_id` and `attempt` in `run.json` match the
directory path. Lower attempts are kept, because they explain why a run took
three tries, but they are never results.

An attempt that was superseded on another rig can still seal after its
replacement started, since the old rig was never told to stop. Both are genuine
measurements of the same spec on comparable hardware. A consumer may show the
lower one until the higher one seals, and must prefer the higher one once it
does.

## Sealing

`manifest.json` is written last. On the filesystem store it is written to a
temporary name in the same directory, fsynced, and renamed into place, so it
either exists completely or not at all. On an object store each object write is
already atomic and it is simply written last.

A directory without `manifest.json` is not a run. Consumers must not read it,
list it, or report it.

```json
{
  "schema_version": "benchgrid.manifest/v1",
  "files": [
    {"path": "run.json", "sha256": "<hex>", "size": 1234},
    {"path": "samples.jsonl", "sha256": "<hex>", "size": 56789}
  ]
}
```

`files` lists every file in the attempt directory except `manifest.json`
itself, sorted by `path`. Any additional files, such as profiler output, are
listed the same way. A consumer verifies every size and digest and treats any
mismatch as a corrupt run.

## Conventions

- Field names are `snake_case`.
- Timestamps are RFC 3339 in UTC, ending in `Z`. benchgrid always writes
  exactly nine fraction digits; a consumer should accept zero to nine.
- `run.json` is a closed schema. A field not listed here is an error, not an
  extension, and adding one is a new schema version.
- Every metric has exactly one unit, fixed per metric per run. Nothing is
  converted on ingest. The unit vocabulary is closed:

| unit | meaning |
| --- | --- |
| `ns` | a duration in nanoseconds, never ms or s |
| `bytes` | a size |
| `ops_per_s` | a rate |
| `ratio` | a fraction in 0..1, never a percentage |
| `celsius` | a temperature |
| `count` | a dimensionless count |
| `unitless` | a dimensionless value that is not a count, such as load average |

## run.json

```json
{
  "schema_version": "benchgrid.run/v1",
  "run_id": "exp_01J...",
  "attempt": 2,
  "fence": 109,
  "status": "SUCCEEDED",
  "status_reason": "",
  "spec_sha256": "<hex>",
  "spec": { "...": "the ExperimentSpec exactly as submitted" },
  "rig": {
    "rig_id": "rig-07",
    "hardware_class": "gpu-a",
    "arch": "x86_64",
    "os": "linux",
    "kernel": "6.8.0",
    "cpu_model": "...",
    "cpu_cores": 16,
    "mem_bytes": 68719476736,
    "gpu_vendor": "",
    "gpu_model": "",
    "gpu_memory_bytes": 0,
    "driver_version": "",
    "firmware": "",
    "emulated": true
  },
  "environment": {
    "git_revision": "<40 hex>",
    "binary_sha256": "<hex>",
    "config_sha256": null,
    "governor": "performance",
    "preflight_before": { "load1": 0.12, "cpu_util": 0.03, "mem_free": 1, "gpu_util": null, "temp_c": null },
    "preflight_after":  { "load1": 0.98, "cpu_util": 0.11, "mem_free": 1, "gpu_util": null, "temp_c": null }
  },
  "timing": {
    "lease_acquired": "2026-10-06T23:40:01.123456789Z",
    "started": "2026-10-06T23:40:02.000000000Z",
    "finished": "2026-10-06T23:40:31.000000000Z"
  },
  "summary": {
    "iteration_latency": {
      "unit": "ns", "n": 30,
      "mean": 0, "median": 0, "p90": 0, "p95": 0, "p99": 0,
      "stddev": 0, "mad": 0, "cv": 0
    }
  }
}
```

### Identity fields

| field | meaning |
| --- | --- |
| `run_id` | the experiment id; there is no separate `experiment_id` field |
| `attempt` | the attempt counter, matches the directory name |
| `fence` | the lease token the attempt ran under, on rig `rig.rig_id`; audit only |
| `spec_sha256` | see below |

### spec_sha256

The lowercase hex sha256 of the RFC 8785 (JCS) canonical serialization of the
`spec` object exactly as it appears in `run.json`. A consumer can and should
recompute it. `internal/canon` implements JCS, including the UTF-16 key order and the
ECMAScript number layout, and is tested against the vectors in RFC 8785. A
JSON library's sorted-keys mode is not a substitute: it orders keys by code
point and formats numbers such as `1e-07` and `100.0` differently.

### status

| status | meaning |
| --- | --- |
| `SUCCEEDED` | every measured repetition ran and the environment stayed valid throughout |
| `FAILED` | the benchmark itself failed: nonzero exit, timeout, crash |
| `INVALID` | the benchmark may have run, but the environment did not hold, so the numbers mean nothing |

`status_reason` is empty for `SUCCEEDED` and a short machine-readable reason
otherwise, for example `timeout`, `preflight:load1`, `preempted:fence`.

`FAILED` and `INVALID` runs still write `samples.jsonl`, possibly partial and
possibly empty. They are evidence of what happened, never inputs to a baseline.

### Identity and provenance

`spec_sha256` identifies the exact experiment, and the spec includes
`revision` and `artifacts`, so it changes with every build. It says which
experiment a run belongs to. It does not say which runs are comparable across
commits, and benchgrid defines no such key: deciding what to compare is the
consumer's job.

`rig.hardware_class`, `rig.arch` and `rig.emulated` say what kind of rig ran
it. `kernel`, `driver_version` and `governor` are recorded as found. An
experiment that needs a specific driver says so in its requirements, and then
no run of it can land on a rig without one.

`environment.git_revision` always equals `spec.revision`, a full 40 character
commit id; the spec rejects an abbreviated one. `environment.binary_sha256`
always equals `spec.artifacts.binary_sha256`, by construction: the agent
hashes the binary it fetched and refuses to run one that differs, which
produces a `FAILED` run with reason `artifact:...` and no samples.
`environment.config_sha256` is 64 hex characters when
`spec.artifacts.config_sha256` is present and `null` when the spec leaves it
out, never an empty string.

`preflight_before` and `preflight_after` use the unit vocabulary: `load1` is
unitless, `cpu_util` and `gpu_util` are ratios, `mem_free` is bytes, `temp_c`
is celsius. A reading the rig cannot take is `null`, never zero.

## samples.jsonl

One JSON object per line, one line per (metric, iteration), warmups included:

```json
{"metric": "iteration_latency", "iteration": 0, "warmup": true, "value": 1834221, "unit": "ns", "t_offset_ns": 0}
```

- `iteration` counts from 0 across warmups and measured repetitions together,
  in execution order, and is contiguous from 0 within each metric.
- Every `metric` and `unit` must be declared in `spec.metrics`, and must match
  the unit of that metric's summary. A consumer rejects a run that has either
  wrong.
- `t_offset_ns` is the time since the first warmup started, on the rig's
  monotonic clock, so drift within a run is visible and not only drift between
  runs.
- Nothing is ever dropped. An outlier is a sample.

## summary

Computed per metric over the samples with `warmup: false` only. With `x` the
measured values sorted ascending and `n` their count:

| field | definition |
| --- | --- |
| `n` | count of measured samples |
| `mean` | arithmetic mean |
| `median` | percentile 50 by the rule below |
| `p90`, `p95`, `p99` | percentiles by the rule below |
| `stddev` | sample standard deviation, divisor `n - 1`; `null` when `n < 2` |
| `mad` | median of `abs(x_i - median)`, raw, not scaled by 1.4826 |
| `cv` | `stddev / mean`; `null` when `stddev` is null or `mean` is 0 |

When `n` is 0, which happens for a `FAILED` or `INVALID` run that measured
nothing, the metric's summary is present with `n: 0` and every other numeric
field `null`. A writer never emits zeros for a quantity it did not measure.

Percentiles use linear interpolation between closest ranks, which is numpy's
default and Hyndman and Fan type 7: for percentile `p` the rank is
`h = (n - 1) * p / 100`, and the value is
`x[floor(h)] + (h - floor(h)) * (x[floor(h) + 1] - x[floor(h)])`, except that
when `floor(h) = n - 1` the value is `x[n - 1]`.

A consumer recomputing these agrees when
`abs(a - b) <= 1e-9 * max(abs(a), abs(b)) + 1e-12`. The absolute term is there
because a purely relative tolerance can never be met when the true value is 0,
as `mad` is on constant data. benchgrid computes in float64 from the same
numbers it wrote to `samples.jsonl`, and `internal/stats` is tested against
numpy output.

## Metric direction

Each metric the spec collects declares whether lower or higher is better,
because a consumer cannot infer it from the unit:

```json
"metrics": [
  {"name": "iteration_latency", "unit": "ns", "direction": "lower_is_better"},
  {"name": "throughput", "unit": "ops_per_s", "direction": "higher_is_better"}
]
```

This lives in the spec, so it is covered by `spec_sha256`, and changing it
makes the runs incomparable, as it should.
