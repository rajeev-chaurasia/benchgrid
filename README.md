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

Every rig here is a process, or a Linux container, on one Apple M4 laptop. No
physical rig and no GPU took part in anything below. Rigs that advertise hardware they do not have are
marked `emulated` in every result they produce, and a spec must opt in before
it can be placed on one.

## The measured result

<!-- evidence:source -->
<!-- /evidence:source -->
Every number below is rendered from those files by
`go run ./script/readme_numbers`, and recomputed from the raw data by
`go run ./script/validate_evidence`. CI runs both on every push: the first
fails if this README quotes a number the evidence does not say, the second if
any summary disagrees with its raw data or either negative control ever stops
failing.

**The lease.**
<!-- evidence:lease -->
<!-- /evidence:lease -->

**The fence.** Three control plane replicas. Replicas freeze themselves with
`SIGSTOP` between leasing a rig and dispatching to it, and are resumed after
the lease has lapsed and the rig has been leased again by someone else. The
run is done twice: with every agent a process on the host, and with every
agent a Linux container in Docker's Linux VM on the same laptop.

<!-- evidence:fence -->
<!-- /evidence:fence -->

The last two columns are the point. The same harness, the same freezes,
against an agent that trusts whatever it is sent, runs two holders' work on one
rig at once. Overlap is computed from every benchmark process and every
session each agent recorded, on the host's monotonic clock, including the
lifetime of any benchmark an agent's death left running.

**Chaos.**
<!-- evidence:chaos -->
<!-- /evidence:chaos -->

## A second, weaker claim

<!-- evidence:noise_claim -->
<!-- /evidence:noise_claim -->

It is weaker on purpose, and here is how.
<!-- evidence:noise -->
<!-- /evidence:noise -->

On this machine the gate is coarse and conservative, and its numbers are
about this machine. The first two designs of the gate failed outright, one by
making the noise worse, and [PLAN.md](PLAN.md) records both.

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
