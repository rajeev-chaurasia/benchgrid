# benchgrid

A control plane for running performance experiments on scarce, exclusive
hardware: it matches each experiment to a rig that satisfies it, leases the
rig exclusively, checks the rig is fit to measure on, runs warmups and
repeated samples, and publishes every raw sample with the provenance needed
to trust it. Go, Postgres, and one agent process per rig.

It is built so its central claim can be checked rather than taken on trust:

> Under concurrent schedulers racing for a fixed pool of rigs, including
> schedulers that are frozen past their lease and then resumed, no rig ever
> executes work for two lease holders at overlapping times, and the same
> harness produces overlapping execution when the fencing check is removed.

The first results below come from one Apple M4 laptop, where every rig is a
process or a Linux container; rigs that advertise hardware they do not have
are marked `emulated` in every result they produce, and a spec must opt in
before it can be placed on one. The section after them comes from a fleet of
cloud VMs in GCP and a real NVIDIA L4, none of them emulated. No physical
bench rig took part in either.

## The measured result

<!-- evidence:source -->
From `evidence/results/20261007T175228Z/`, at commit `1e07558`, on Postgres 14.18 (Homebrew) and an
Apple M4 whose own background load kept 25% of its CPU busy before any run started.
<!-- /evidence:source -->
Every number below is rendered from those files by
`go run ./script/readme_numbers`, and recomputed from the raw data by
`go run ./script/validate_evidence`. CI runs both on every push: the first
fails if this README quotes a number the evidence does not say, the second if
any summary disagrees with its raw data or either negative control ever stops
failing.

**The lease.**
<!-- evidence:lease -->
64 workers, 20 rigs, 50,000 acquisition attempts per mode, TTLs of 5 to 20 ms,
one grant in ten left to expire.

| acquire | grants | double bookings | peak attempts in flight |
| --- | ---: | ---: | ---: |
| one conditional `UPDATE` (the product) | 2,193 | **0** | 63 |
| read, then unconditional write (control) | 5,894 | 40,262 | 64 |
<!-- /evidence:lease -->

**The fence.** Three control plane replicas. Replicas freeze themselves with
`SIGSTOP` between leasing a rig and dispatching to it, and are resumed after
the lease has lapsed and the rig has been leased again by someone else. The
run is done twice: with every agent a process on the host, and with every
agent a Linux container in Docker's Linux VM on the same laptop. In the Linux
run, rigs are also cut off from the network for longer than their lease while
they keep running, which a host process cannot be.

<!-- evidence:fence -->
| rigs | agent | experiments | freezes | partitions | stale dispatches that reached a rig | refused | overlapping process pairs | overlapping session pairs |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 8 host processes | checks the fence | 300 | 38 | n/a | 51 | 51 | **0** | **0** |
| 8 host processes | does not (control) | 300 | 32 | n/a | 35 | 0 | 38 | 45 |
| 6 Linux containers | checks the fence | 300 | 44 | 15 | 43 | 43 | **0** | **0** |
| 6 Linux containers | does not (control) | 300 | 31 | 14 | 27 | 0 | 58 | 40 |
<!-- /evidence:fence -->

The last two columns are the point. The same harness, the same freezes,
against an agent that trusts whatever it is sent, runs two holders' work on one
rig at once. Overlap is computed from every benchmark process and every
session each agent recorded, on the host's monotonic clock, including the
lifetime of any benchmark an agent's death left running.

**Chaos.**
<!-- evidence:chaos -->
3 replicas, 8 rigs in four emulated hardware classes, 600 experiments,
48 of them built to fail. During the run: 52 agents killed and restarted, 14
replicas killed and restarted, 28 replicas frozen, 8 outages of the whole
control plane at once, and one artifact write in five refused. Those random
faults, with nothing aimed, produced 4 stale dispatches, every one refused
at the rig.

| | |
| --- | ---: |
| experiments ending as they should (sound ones succeed, broken ones fail) | 600 of 600 |
| final attempts with a sealed artifact that verifies and agrees with the control plane | 600 of 600 |
| runs placed on a rig their spec did not allow | 0 |
| overlapping process pairs | 0 |
| rigs still leased afterwards | 0 |
| experiments needing more than one attempt | 50 with 2, 6 with 3, 1 with 4 |
<!-- /evidence:chaos -->

## A second, weaker claim

<!-- evidence:noise_claim -->
> With bursty load injected on its host, the measurement gate declined to
> publish 12 of 12 runs that an ungated agent published with a median
> coefficient of variation of 49.5%, against 38.5% with no injected load.
<!-- /evidence:noise_claim -->

It is weaker on purpose, and here is how.
<!-- evidence:noise -->
No loaded run got through the gate. The gate also declined 12 of
12 runs with no injected load, because the host's own background load,
25% of its CPU before the runs began, crossed the limit during them. A
gate that refuses that often on an idle machine is not one anybody would
leave switched on here.

| condition | published | declined | median CV of published | median latency |
| --- | ---: | ---: | ---: | ---: |
| no load | 12 | 0 | 38.5% | 99.9 ms |
| no load, gated | 0 | 12 | n/a | n/a |
| bursty load | 12 | 0 | 49.5% | 87.0 ms |
| bursty load, gated | 0 | 12 | n/a | n/a |
<!-- /evidence:noise -->

On this machine the gate is coarse and conservative, and its numbers are
about this machine. The first two designs of the gate failed outright, one by
making the noise worse, and [PLAN.md](PLAN.md) records both.

## On real machines: a GCP fleet

Everything above runs on one laptop. This section is a fleet in GCP built by
`deploy/gcp`: the control plane on GKE with its run store in Cloud Storage,
bench nodes provisioned by a startup script with kernel isolation, a GCE
synchronised clock and the agent under systemd, a real NVIDIA GPU node, and
every binary built from the published commit by Cloud Build. Each study
publishes every run it caused, and `benchgrid-study validate` recomputes its
numbers from them in CI.

<!-- evidence:gcp_source -->
- `evidence/results/20261008T020541Z-gcp/`, published at commit `a76802c`: 3 tuned and 1 default bench nodes (n2d-standard-4, one thread per core, AMD EPYC 7B13,
  kernel 6.1.0-53-cloud-amd64), control plane on GKE 1.35.8-gke.1225000, two replicas, Postgres 16 in cluster.
<!-- /evidence:gcp_source -->

**The CI gate.** `benchgrid-gate` runs baseline and candidate in pairs,
alternating which goes first, reruns any run noisier than 5%, and calls a
regression only when the whole 95% bootstrap interval is above no change and
the estimate is at least 2% slower. Evaluated on every avbench profile:

<!-- evidence:gcp_gate -->
240 comparisons over the 30 avbench profiles on tuned nodes: 180 null, where
baseline and candidate are the same binary, and 60 with an injected slowdown.
1,986 runs, 80 of them reruns of a run noisier than 5%; 396 of 993 pairs ran on
one rig.

| comparison | comparisons | REGRESSION | PASS | INCONCLUSIVE | other |
| --- | ---: | ---: | ---: | ---: | ---: |
| null (no change) | 180 | 8 | 145 | 26 | 1 |
| injected 3% | 15 | 12 | 1 | 2 | 0 |
| injected 5% | 15 | 13 | 0 | 1 | 1 |
| injected 8% | 15 | 15 | 0 | 0 | 0 |
| injected 12% | 15 | 15 | 0 | 0 | 0 |

On the null comparisons, 8 of 180 were called a regression: a false alarm rate
of 4.4%. Of the injected ones, 55 of 60 were caught.
<!-- /evidence:gcp_gate -->

**The GPU.**

<!-- evidence:gcp_gpu -->
On a real NVIDIA L4, 23034 MiB, driver 580.178.04, not emulated. Before each run the agent read the GPU's
temperature at 47 C to 52 C and its utilization at 0% through nvidia-smi, and would
have waited or refused above 85 C or 10%.

| batch of frames | runs | not succeeded | GPU time per forward pass | frames per second | median CV |
| --- | ---: | ---: | ---: | ---: | ---: |
| 4 at 192x192 | 3 | 0 | 2.77 ms | 1446 | 1.8% |
| 8 at 256x256 | 3 | 0 | 2.73 ms | 2927 | 2.5% |
| 16 at 320x320 | 3 | 0 | 7.32 ms | 2186 | 0.4% |

| gate comparison on the GPU | verdict | change | 95% interval | pairs |
| --- | --- | ---: | --- | ---: |
| null | PASS | +0.1% | -5.8% to +1.6% | 3 |
| depth 2 to 3 | REGRESSION | +34.8% | +33.5% to +36.5% | 3 |
<!-- /evidence:gcp_gpu -->

**Kernel isolation, and the result that went against the plan.** Tuned nodes
pin every benchmark to a core the kernel isolates, in a cgroup of its own;
default nodes are the same machine type with nothing tuned. A noise source on
core 0 is switched on and off for the whole fleet, alternating per profile.

<!-- evidence:gcp_isolation -->
| nodes | noise on the system core | runs | not succeeded | median of per-profile median CV |
| --- | --- | ---: | ---: | ---: |
| SMT off, nothing isolated | off | 120 | 0 | 0.5% |
| SMT off, nothing isolated | on | 120 | 0 | 0.9% |
| tuned: SMT off, isolated core, pinned | off | 120 | 0 | 0.9% |
| tuned: SMT off, isolated core, pinned | on | 120 | 0 | 1.0% |

| rig | class | median CV, noise off | p90 CV, noise off | median CV, noise on | p90 CV, noise on |
| --- | --- | ---: | ---: | ---: | ---: |
| benchgrid-rig-default-0 | SMT off, nothing isolated | 0.5% | 1.1% | 0.8% | 2.1% |
| benchgrid-rig-tuned-0 | tuned | 0.5% | 1.0% | 0.6% | 1.3% |
| benchgrid-rig-tuned-1 | tuned | 0.8% | 2.0% | 0.7% | 1.3% |
| benchgrid-rig-tuned-3 | tuned | 3.1% | 6.3% | 2.4% | 4.6% |
<!-- /evidence:gcp_isolation -->

Isolation did not reduce variation on these VMs: with one thread per core,
both classes were already well under one percent, and the rig by rig numbers
show the largest effect is one VM several times noisier than identical peers,
which nothing set inside a VM can fix. So benchgrid now measures it.

**Noisy rigs, benched.** Every idle agent runs a fixed calibration canary on
its bench core and reports the spread; a spec that sets `max_rig_noise_cv` is
never placed on a rig noisier than that, or on one not yet measured. Jobs with
and without that limit, interleaved on the same fleet:

<!-- evidence:gcp_canary -->
| rig | canary CV, median and range | jobs placed with `max_rig_noise_cv` 1% | jobs placed with no limit |
| --- | --- | ---: | ---: |
| benchgrid-rig-default-0 | 0.65% (0.31% to 1.54%, 4 readings) | 15 | 6 |
| benchgrid-rig-tuned-0 | 0.33% (0.31% to 0.36%, 4 readings) | 12 | 8 |
| benchgrid-rig-tuned-1 | 0.47% (0.44% to 0.66%, 4 readings) | 13 | 7 |
| benchgrid-rig-tuned-3 | 1.96% (0.43% to 2.05%, 4 readings) | 0 | 19 |

40 of 40 limited jobs and 40 of 40 unlimited ones succeeded.
<!-- /evidence:gcp_canary -->

**Scale.**

<!-- evidence:gcp_scale -->
| | |
| --- | ---: |
| jobs | 12,000 |
| outcomes | 12,000 SUCCEEDED |
| succeeded | 100.00% |
| throughput | 200 jobs/minute |
| lease to finished, median and p95 | 1.1 s and 1.3 s |
| wall clock | 3601.6 s |

Every job was submitted at once, so queue wait measures the backlog draining
(median 1730.9 s), not the scheduler: a job's own time from lease to finished,
including preflight, the run, and sealing its results in Cloud Storage, is
the row above.
<!-- /evidence:gcp_scale -->

Every one of those runs is also in BigQuery, loaded by `benchgrid-bq` into
tables partitioned by day and clustered by benchmark, hardware class and
metric.

<!-- evidence:gcp_bigquery -->
The tables hold 14688 runs, 14676 of them succeeded, from 5 rigs across 33
benchmarks. The last export verified every attempt against its manifest
before loading it and found 0 corrupt; the queries and what they returned
are in `bigquery/`.
<!-- /evidence:gcp_bigquery -->

## How the guarantee works

Two mechanisms, and the second is the one most designs leave out.

**A lease granted in one statement.** A rig's row holds `holder`,
`expires_at`, and a `fence` that only ever increases. Acquisition is a single
`UPDATE ... WHERE holder IS NULL OR expires_at < clock_timestamp()` that also
increments the fence, so there is no window between seeing a rig free and
taking it, and expiry is judged on the database clock rather than any
scheduler's. The experiment claim and its attempt counter change in the same
transaction.

**A fence enforced at the rig.** A lease cannot stop a scheduler that was
frozen past its TTL from waking up and dispatching anyway, because the
scheduler does not know it was frozen. So every dispatch carries the fence,
and the agent keeps the highest one it has seen, fsynced before use. A lower
fence is refused. A higher one first kills and reaps everything running under
the old one, then proceeds. This is the fencing token pattern; without it the
lease is advice.

Leases are renewed by the rig's own heartbeat, conditioned on the fence, so a
scheduler crash loses nothing, and a lease that lapsed during a control plane
outage resumes if nobody took it over. The reaper requeues an attempt only
once the rig has shown it is not running it.

The reasons behind each of these are in [docs/adr](docs/adr).

## What a run produces

Every attempt, successful or not, is a sealed directory:

```
runs/<experiment>/attempt-<n>/run.json        spec, rig, provenance, summary
                              samples.jsonl   every iteration, warmups flagged
                              manifest.json   sha256 of both, written last
```

`run.json` carries the spec and its sha256, the rig as it described itself
(including `emulated`), the commit, the binary's sha256, the preflight
readings before and after, and per metric `n`, mean, median, p90, p95, p99,
standard deviation, MAD, and CV. The summary is a checked claim: it is
defined exactly in [docs/run-artifact.md](docs/run-artifact.md), tested
against numpy, and recomputed from the samples by the verifier.

## Running it

```bash
make build
```

```bash
./bin/benchgrid -db 'postgres:///benchgrid?sslmode=disable' -artifacts var/store
```

```bash
./bin/rigagent -id rig-01 -state-dir var/rig-01 -endpoint http://127.0.0.1:9090
```

```bash
./bin/bgctl submit -spec examples/cpu_hash.json -binary bin/benchload -key ci-$GIT_SHA -wait
```

`bgctl` exits 0 for `SUCCEEDED`, 1 for `FAILED` or `INVALID`, and 2 when it
could not find out, so a pipeline can tell a broken benchmark from a broken
pipeline.

Reproduce the evidence with `make evidence`, which takes a while and needs a
local Postgres and Docker. Check it with `make validate`, and render this
README's numbers from it with `make readme`. Method and limits:
[docs/evidence.md](docs/evidence.md), [docs/known-misses.md](docs/known-misses.md),
[docs/non-goals.md](docs/non-goals.md).

## Layout

```
cmd/benchgrid          control plane: API, scheduler, reaper
cmd/rigagent           one per rig: fence, preflight, execution, spool
cmd/bgctl              CI client
cmd/benchload          the benchmark the evidence measures
cmd/evidence           produces evidence/results
internal/lease         the lease, and nothing else
internal/agent         the rig agent
internal/sched         placement, completion, reaping
internal/artifact      the run contract: write, seal, verify
internal/evidence      every tally the harness and validator share
script/validate_evidence
```
