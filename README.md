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

Every rig here is a process on one Apple M4 laptop. No physical rig and no GPU
took part in anything below. Rigs that advertise hardware they do not have are
marked `emulated` in every result they produce, and a spec must opt in before
it can be placed on one.

## The measured result

From `evidence/results/20261007T002030Z/`, at commit `9c2b23c`, on Postgres
14.18. Every number is recomputed from the raw files beside it by
`go run ./script/validate_evidence`, which CI runs on every push and which
fails if either negative control ever stops failing.

**The lease.** Sixty-four workers, twenty rigs, fifty thousand acquisition
attempts per mode, TTLs of 5 to 20 ms, one grant in ten left to expire.

| acquire | grants | double bookings | peak attempts in flight |
| --- | ---: | ---: | ---: |
| one conditional `UPDATE` (the product) | 2,436 | **0** | 62 |
| read, then unconditional write (control) | 5,641 | 40,445 | 64 |

**The fence.** Three control plane replicas, eight rigs, three hundred
experiments per mode. Replicas freeze themselves with `SIGSTOP` between
leasing a rig and dispatching to it, and are resumed after the lease has
lapsed and the rig has been leased again by someone else.

| agent | freezes | stale dispatches that reached a rig | refused | overlapping process pairs | overlapping session pairs |
| --- | ---: | ---: | ---: | ---: | ---: |
| checks the fence | 34 | 32 | 32 | **0** | **0** |
| does not (control) | 40 | 35 | 0 | 83 | 46 |

The last two columns are the point. The same harness, the same freezes,
against an agent that trusts whatever it is sent, runs two holders' work on one
rig at once. Overlap is computed from every benchmark process and every
session each agent recorded, on the host's monotonic clock.

**Chaos.** Three replicas, eight rigs in four emulated hardware classes, six
hundred experiments, forty-eight of them built to fail. During the run: 49
agents killed and restarted, 14 replicas killed and restarted, 23 replicas
frozen, 7 outages of the whole control plane at once, and one artifact write
in five refused.

| | |
| --- | ---: |
| experiments ending as they should (sound ones succeed, broken ones fail) | 600 of 600 |
| final attempts with a sealed artifact that verifies and agrees with the control plane | 600 of 600 |
| runs placed on a rig their spec did not allow | 0 |
| overlapping process pairs | 0 |
| rigs still leased afterwards | 0 |
| experiments needing a second or third attempt | 36 and 4 |

## A second, weaker claim

> With bursty load injected on its host, the measurement gate declined to
> publish 11 of 12 runs that an ungated agent published with a median
> coefficient of variation of 23%, against 4.7% with no injected load.

It is weaker on purpose, and here is how. One loaded run got through, with a
CV of 8.4%. The gate also declined 4 of 12 runs with no injected load, because
this laptop's own background load, about a fifth of its CPU when idle,
crossed the limit during them. On this machine the gate is coarse and
conservative, and its numbers are about this machine. The first two designs
of the gate failed outright, one by making the noise worse, and
[PLAN.md](PLAN.md) records both.

| condition | published | declined | median CV of published | median latency |
| --- | ---: | ---: | ---: | ---: |
| no load | 12 | 0 | 4.7% | 37.7 ms |
| no load, gated | 8 | 4 | 1.3% | 37.4 ms |
| bursty load | 12 | 0 | 23.4% | 46.2 ms |
| bursty load, gated | 1 | 11 | 8.4% | 44.7 ms |

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

Reproduce the evidence with `make evidence` (about eleven minutes here, needs a
local Postgres) and check it with `make validate`. Method and limits:
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
