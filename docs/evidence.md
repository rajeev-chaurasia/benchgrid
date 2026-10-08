# Evidence

Everything under `evidence/results/<stamp>/` is produced by one command,
`make evidence`, and checked by another, `make validate`, which CI runs on
every push. The validator recomputes every summary number from the raw files
beside it, checks a `SHA256SUMS` manifest that `sha256sum -c` can also check,
applies the run contract to every sealed run artifact, and fails if the claim
does not hold or if either negative control stops failing.

`env.json` records the commit, Go and Postgres versions, and the host. Every
rig in every run is a process on that one host.

## lease_race

Twenty rigs, sixty-four workers released from one gate, fifty thousand
acquisition attempts per mode. Each worker picks a rig at random, tries to
lease it with a TTL between 5 and 20 ms, and if it wins, holds for up to 3 ms
and releases, except one grant in ten, which is abandoned to expire so that
takeover after expiry is raced too.

Two modes, identical in everything but the acquire statement:

- `conditional` is `internal/lease.Acquire`, the one the product uses.
- `naive` is a read of whether the rig is free followed by an unconditional
  write, the shape every hand-rolled lock starts as. It lives in
  `cmd/evidence`, so nothing in the product can call it.

Published per mode: every grant as an interval on the database clock, every
attempt as an interval on the client clock, and the summary. A double booking
is a pair of grants on one rig, held by different holders, whose intervals
intersect. `peak_in_flight` is the largest number of acquire statements in
progress at one instant, published so a reader can see the race raced.

## fence

Three control plane replicas, eight agents, three hundred experiments, lease
TTL 3 s. Each replica stops itself with `SIGSTOP`, with probability 0.1,
after committing a lease and before dispatching it, which is the one point
where a freeze turns into a stale dispatch. The harness detects each freeze
and resumes the replica after the TTL plus one to three seconds, long enough
for the lease to lapse, the experiment to be requeued, and the rig to be
leased again by another replica.

Two modes: agents that check the fence, and agents started with
`-unfenced-negative-control`, which accept everything but still record when a
stale fence arrives. Published per mode: every interval every agent recorded
(each benchmark process, each session from acceptance to cleanup, each stale
arrival), every freeze, and the summary. Overlap is counted between intervals
on one rig under different fences, on the host's monotonic clock, so no
cross-machine clock agreement is assumed.

The validator requires zero overlapping pairs in the fenced mode, at least one
stale fence refused in the fenced mode (or the freezes never landed), and at
least one overlapping pair in the control.

## linux

The fence run again, with six agents running as Linux containers in Docker's
Linux VM on the same host, built from a scratch image holding only the agent
and the benchmark. Everything else is identical, including the control, with
one addition: every two to five seconds a random rig container is disconnected
from the network for the lease TTL plus one to five seconds and then
reconnected. The agent keeps running throughout, so its lease lapses, its
attempt is requeued, and it reports a result late, which is what a partition
does and a crash does not. The
Linux runs also publish their run artifacts, and the validator checks from
each one that its rig reports Linux and that every `max_rss` sample is a
plausible number of bytes, which is how a kilobyte figure read as bytes would
show.

## chaos

Three replicas, eight agents in four emulated hardware classes, six hundred
experiments whose requirements need each class. Twenty four of them are
built to fail: half crash by signal, half exit nonzero. While they run, the
harness applies one fault every one to three seconds:

- `agent_kill`: `SIGKILL` an agent, restart it one to three seconds later on
  the same state directory.
- `replica_kill`: `SIGKILL` a replica, restart it one to two seconds later.
- `replica_freeze`: `SIGSTOP` a replica for two to five seconds.
- `control_plane_outage`: `SIGSTOP` every replica at once for four to eight
  seconds while agents keep running.

and every replica answers one artifact write in five with a 503.

Stale dispatches refused during chaos are counted: these come from random
faults, not aimed ones.

An experiment recovered if it ended in the state its kind should produce:
`SUCCEEDED` for a sound benchmark, `FAILED` for a broken one, which shows that
recovery does not retry a real failure into a false success. For every
experiment, the sealed artifact of its final attempt must exist, verify under
the run contract, record the status the control plane reports, and show a rig
that satisfies its spec. Every miss and every artifact error is listed by
id in the summary rather than counted only.

## noise

One agent, one replica, a fixed SHA-256 benchmark of about 40 ms, five warmups
and thirty measured repetitions per run, twelve runs per condition, one run at
a time, in four conditions:

- `quiet`: no injected load. Not idle: the development host keeps roughly a
  fifth of its CPU busy with other software, measured at about 2.1 busy
  core-seconds per second on ten cores, and that stays in every condition.
- `loaded_ungated`: a seeded noise source spins every core in bursts of 0.3 to
  1.5 s separated by idle gaps of the same range.
- `quiet_gated`: no injected load, with the gate on, which measures how
  often the gate rejects a machine with nothing wrong with it.
- `loaded_gated`: the same noise as `loaded_ungated`, with `max_cpu_util` 0.4
  and `gate_during_measurement`, so the agent measures the background load
  across the measured iterations, less the benchmark's own CPU time, and
  declares the run `INVALID` if it exceeded the limit.

The conditions are interleaved run by run, not run in blocks, because the
host's own load drifts over minutes and in blocks the drift reads as a
difference between conditions. Each run gets exactly one attempt, so an `INVALID` verdict is not retried into
a second sample of the same noise.

Published: every run artifact, and per condition the number of runs that
succeeded and that were declared invalid, and the median and maximum CV of
iteration latency over the succeeded runs. An invalid run is the gate
refusing to produce a number, and is counted, never averaged in.

## GCP

Everything under `evidence/results/<stamp>-gcp/` was measured on a fleet in
GCP project infrastructure built by `deploy/gcp`: the control plane on a
one-node GKE cluster, with Postgres in the cluster and the run store in a
Cloud Storage bucket; bench nodes as `n2d-standard-4` VMs with one thread per
core, so each has two physical cores; and one GPU node. Every binary was built
from the published commit by Cloud Build. `env.json` records the fleet as it
described itself. Each study writes every run it caused, sealed files and all,
and `benchgrid-study validate` recomputes its summary from them in CI.

**Tuned and default nodes.** A tuned node boots with its second core isolated
from the scheduler (`isolcpus`, `nohz_full`, `rcu_nocbs`), interrupts steered
off it, transparent huge pages off, and its clock synchronised to Google's
metadata server; the agent pins each benchmark to that core, in a cgroup that
owns it, and refuses to run if the clock's error bound exceeds 5 ms. A default
node is the same machine type with none of that. A noise source on every
node, switched by instance metadata, keeps core 0 busy in bursts.

**gate.** The CI gate's evaluation: for each of the 30 avbench profiles, six
null comparisons, where baseline and candidate are the same binary, and two
with an injected slowdown of 3, 5, 8 or 12 percent, run on tuned nodes. A
REGRESSION on a null comparison is a false alarm.

**isolation.** For each profile, four runs on each class with the noise off
and four with it on, the conditions alternating per profile. Published per
class and condition, and per rig, because the per-rig numbers decide whether a
class difference is the tuning or one machine.

**tuning.** The isolation study again, against stock nodes: the same machine
type as GCE ships it, SMT on and nothing isolated. Two tuned and two stock
nodes in us-west1-b and the same in us-central1-a.

**hil.** `avbench --loop` at 50 Hz, 500 cycles a session, on four perception
kernels, on tuned and stock nodes with the noise off and on. Each cycle wakes
on an absolute schedule and processes the next frame of a recording made
before the loop starts; deadline misses, cycle times and wake-up lateness are
published per session.

**gate_soft and gate_strict.** Two gate evaluations at the same time on the
same tuned nodes, 150 comparisons each: one with soft affinity and no noise
limit, one with pairs required to share a rig and rigs with a canary CV above
1.2% excluded.

**gpu.** The perception workload in `workloads/gpubench` on the GPU node, at
three batch sizes, gated on the GPU's own temperature and utilization; and the
CI gate on the GPU, once with no change and once with a real one: a model with
one more residual block per stage.

**scale.** Short replayed jobs across every bench node, measuring how many
ended well, how long they queued, and how many the fleet completed per minute,
from the control plane's own timestamps.

**bigquery.** The exporter's report and three queries with what they
returned. Unlike everything else here, these cannot be recomputed in CI,
which has no access to the warehouse; they are a record of what the tables
held, and the queries are there to be run again by anyone who has.

## What none of this shows

See `known-misses.md`. In short: no physical rig and no GPU took part, the
fence run aims its freezes, and the noise run is about this machine.
