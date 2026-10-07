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

Each run gets exactly one attempt, so an `INVALID` verdict is not retried into
a second sample of the same noise.

Published: every run artifact, and per condition the number of runs that
succeeded and that were declared invalid, and the median and maximum CV of
iteration latency over the succeeded runs. An invalid run is the gate
refusing to produce a number, and is counted, never averaged in.

## What none of this shows

See `known-misses.md`. In short: no physical rig and no GPU took part, the
fence run aims its freezes, and the noise run is about this machine.
