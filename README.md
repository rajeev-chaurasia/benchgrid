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
From `evidence/results/20261007T035423Z/`, at commit `c27816b`, on Postgres 14.18 (Homebrew) and an
Apple M4 whose own background load kept 33% of its CPU busy before any run started.
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
| one conditional `UPDATE` (the product) | 2,097 | **0** | 64 |
| read, then unconditional write (control) | 6,206 | 46,637 | 64 |
<!-- /evidence:lease -->

**The fence.** Three control plane replicas. Replicas freeze themselves with
`SIGSTOP` between leasing a rig and dispatching to it, and are resumed after
the lease has lapsed and the rig has been leased again by someone else. The
run is done twice: with every agent a process on the host, and with every
agent a Linux container in Docker's Linux VM on the same laptop. In the Linux
run, rigs are also cut off from the network for longer than their lease while
they keep running, which a host process cannot be.

<!-- evidence:fence -->
| rigs | agent | experiments | freezes | stale dispatches that reached a rig | refused | overlapping process pairs | overlapping session pairs |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 8 host processes | checks the fence | 300 | 38 | 38 | 38 | **0** | **0** |
| 8 host processes | does not (control) | 300 | 35 | 37 | 0 | 86 | 57 |
| 6 Linux containers | checks the fence | 300 | 47 | 46 | 46 | **0** | **0** |
| 6 Linux containers | does not (control) | 300 | 31 | 37 | 0 | 96 | 45 |
<!-- /evidence:fence -->

The last two columns are the point. The same harness, the same freezes,
against an agent that trusts whatever it is sent, runs two holders' work on one
rig at once. Overlap is computed from every benchmark process and every
session each agent recorded, on the host's monotonic clock, including the
lifetime of any benchmark an agent's death left running.

**Chaos.**
<!-- evidence:chaos -->
3 replicas, 8 rigs in four emulated hardware classes, 600 experiments,
48 of them built to fail. During the run: 51 agents killed and restarted, 14
replicas killed and restarted, 27 replicas frozen, 7 outages of the whole
control plane at once, and one artifact write in five refused. Those random
faults, with nothing aimed, produced no stale dispatch at all, which is why
the fence run has to aim its freezes to test the fence.

| | |
| --- | ---: |
| experiments ending as they should (sound ones succeed, broken ones fail) | 600 of 600 |
| final attempts with a sealed artifact that verifies and agrees with the control plane | 600 of 600 |
| runs placed on a rig their spec did not allow | 0 |
| overlapping process pairs | 0 |
| rigs still leased afterwards | 0 |
| experiments needing more than one attempt | 39 with 2, 2 with 3, 1 with 4 |
<!-- /evidence:chaos -->

## A second, weaker claim

<!-- evidence:noise_claim -->
> With bursty load injected on its host, the measurement gate declined to
> publish 12 of 12 runs that an ungated agent published with a median
> coefficient of variation of 16.9%, against 4.3% with no injected load.
<!-- /evidence:noise_claim -->

It is weaker on purpose, and here is how.
<!-- evidence:noise -->
No loaded run got through the gate. The gate also declined 8 of
12 runs with no injected load, because the host's own background load,
33% of its CPU before the runs began, crossed the limit during them. A
gate that refuses that often on an idle machine is not one anybody would
leave switched on here.

| condition | published | declined | median CV of published | median latency |
| --- | ---: | ---: | ---: | ---: |
| no load | 12 | 0 | 4.3% | 42.0 ms |
| no load, gated | 4 | 8 | 1.2% | 41.1 ms |
| bursty load | 12 | 0 | 16.9% | 46.1 ms |
| bursty load, gated | 0 | 12 | n/a | n/a |
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
