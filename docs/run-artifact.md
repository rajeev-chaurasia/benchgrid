# Run artifact contract, v1

This is the boundary between benchgrid and TraceLab. benchgrid writes it and
TraceLab reads it, and nothing else passes between them. Everything below is
normative. Where this file and the code disagree, the code is wrong.

The summary statistics in a run are a checked claim, not the source of truth.
TraceLab recomputes every one of them from the raw samples and rejects the run
if they disagree, so the rules for computing them are spelled out exactly.

## Layout

```
<store>/runs/<run_id>/attempt-<fence>/
    run.json
    samples.jsonl
    manifest.json
```

`run_id` is the experiment id, so every attempt at one experiment lands under
one directory. `fence` is the lease token the attempt ran under, written in
decimal with no padding. Two attempts can never share a directory, because two
attempts can never hold the same token.

A consumer ingests only the sealed attempt with the highest fence for a
`run_id`. Lower attempts are kept, because they explain why a run took three
tries, but they are never results.

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
- Timestamps are RFC 3339 with nanoseconds, in UTC, ending in `Z`.
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
  "fence": 109,
  "status": "SUCCEEDED",
  "status_reason": "",
  "spec_sha256": "<hex>",
  "spec": { "...": "the ExperimentSpec exactly as submitted" },
  "rig": {
    "rig_id": "rig-07",
    "hardware_class": "gpu-a",
    "arch": "x86_64",
    "cpu_model": "...",
    "cpu_cores": 16,
    "mem_bytes": 68719476736,
    "gpu_model": "",
    "driver_version": "",
    "kernel": "6.8.0",
    "os": "linux",
    "emulated": true
  },
  "environment": {
    "git_revision": "<40 hex>",
    "binary_sha256": "<hex>",
    "config_sha256": "<hex>",
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

### Comparability

`spec_sha256`, `rig.hardware_class`, `rig.arch` and `rig.emulated` together say
whether two runs measured the same thing on the same kind of hardware.
`kernel`, `driver_version` and `governor` are recorded so a consumer can
annotate a comparison, and benchgrid does not treat a difference in them as
making two runs incomparable. An experiment that needs a specific driver says
so in its requirements, and then no run of it can land on a rig without one.

`preflight_before` and `preflight_after` use the unit vocabulary: `load1` is
unitless, `cpu_util` and `gpu_util` are ratios, `mem_free` is bytes, `temp_c`
is celsius. A reading the rig cannot take is `null`, never zero.

## samples.jsonl

One JSON object per line, one line per iteration, warmups included:

```json
{"metric": "iteration_latency", "iteration": 0, "warmup": true, "value": 1834221, "unit": "ns", "t_offset_ns": 0}
```

- `iteration` counts from 0 across warmups and measured repetitions together,
  in execution order.
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
| `stddev` | sample standard deviation, divisor `n - 1`; `0` when `n < 2` |
| `mad` | median of `abs(x_i - median)`, raw, not scaled by 1.4826 |
| `cv` | `stddev / mean`; `null` when `mean` is 0 |

Percentiles use linear interpolation between closest ranks, which is numpy's
default and Hyndman and Fan type 7: for percentile `p` the rank is
`h = (n - 1) * p / 100`, and the value is
`x[floor(h)] + (h - floor(h)) * (x[floor(h) + 1] - x[floor(h)])`.

A consumer recomputing these should agree to within `1e-9` relative. benchgrid
computes in float64 from the same integers it wrote to `samples.jsonl`.

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
