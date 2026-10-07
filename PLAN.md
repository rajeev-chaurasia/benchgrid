# benchgrid, implementation plan

## 0. Revisions

Corrected against what the build and its evidence runs established, rather
than left as first written. The substantive changes:

- **The fence was the attempt identity in the first contract.** It cannot
  be: it is counted per rig, so a retry elsewhere can carry a smaller one.
  Attempts are now counted on the experiment. ADR 0004.
- **The scheduler was going to renew leases.** The rig does, through its
  heartbeat, so a scheduler crash loses nothing. ADR 0002.
- **The reaper requeued on any lapsed lease.** After a control plane outage
  every lease has lapsed with the work still running, so it now needs proof
  that the attempt is dead, and it settles after its own pauses.
- **Agents tracked their benchmark process groups only in memory.** An agent
  killed mid-run left its benchmark running beside the next measurement. The
  groups are now recorded on disk and reaped at startup.
- **Interval times were relative to the agent process.** A restarted agent
  restarted the clock, which would have made intervals from before and after
  a restart incomparable. They now use the host's monotonic clock.
- **The gate ran only at preflight.** Bursty load that starts afterwards
  passed straight through, so a spec can now gate every iteration.
- **Gating before each iteration made things worse.** The first noise trial
  measured a median CV of 21% with that gate against 17% without it, and a
  higher median latency, with every loaded run published as a success. A gate
  that waits for a quiet window passes late in each quiet gap, which lines
  iterations up with the onset of the next burst.
- **Checking each iteration on its own then rejected every quiet run.** The
  macOS CPU counters cannot judge a 40 ms window: quiet iterations measured
  impossible background loads up to eight times the machine. The gate now
  measures background load across the whole measured phase, less the
  benchmark's own CPU time. It is coarser, and the evidence says so: a loaded
  run that happens to catch few bursts can pass.
- **The noise conditions ran in blocks.** The host's own load drifted between
  blocks enough to look like a difference between conditions. They are now
  interleaved run by run.
- **The second claim was rewritten** around what the third design can show,
  after it was measured, which is stated in the README rather than hidden.
- **The first unfenced control showed no overlap at all.** Each new session's
  preflight was killing every process the agent had ever launched, including
  the other session's, which is accidental fencing. Stale process reaping is
  now limited to sessions that have finished, and the control overlaps as it
  should. Without the control, the fenced result would have looked identical
  and proved nothing.
- **A scheduler waking from a freeze could resurrect an abandoned attempt**
  by marking it dispatched, and an agent would then retry a refused
  completion forever. Both were found by the first fence trial, not by the
  unit tests, and both now have tests that fail without the fix.

## 1. The claim this repo has to earn

A benchmark scheduler is easy to write and hard to believe. Every one of them
says it gives a job exclusive access to a machine, and almost none of them can
show what happens when the scheduler that held the machine stops for longer
than its lease and then carries on as if nothing happened. A TTL lock covers the
case where the holder dies. It does nothing for the case where the holder only
paused, and a paused holder that wakes up and keeps issuing commands is how two
benchmarks end up running on one rig at once, and how a number gets published
that belongs to neither of them.

This project makes one claim and spends most of its effort making it
falsifiable:

> Under concurrent schedulers racing for a fixed pool of rigs, including
> schedulers that are frozen past their lease and then resumed, no rig ever
> executes work for two lease holders at overlapping times, and the same harness
> produces overlapping execution when the fencing check is removed.

Three things follow from that wording, and they drive the design:

1. **Execution, not bookkeeping.** A lease table with no duplicate rows proves
   the database works. The thing that has to be exclusive is what the rig
   actually ran, so every agent records the start and end of every process it
   launched, with the token that launched it, on its own monotonic clock, and
   overlap is computed from those raw intervals.
2. **Frozen, not killed.** Killing a scheduler is the easy failure, because a
   dead process issues no more commands. The harness freezes scheduler
   processes with `SIGSTOP` past the lease TTL and resumes them with `SIGCONT`,
   which is the failure a TTL alone cannot handle.
3. **A negative control.** The same harness, with the same freezes, runs against
   an agent that does not check the token. It has to produce overlapping
   execution, and if it ever stops doing so the evidence run fails, because that
   would mean the freezes stopped landing where they matter.

There is a second claim, weaker and stated separately in the README because a
combined claim would be weaker than the first one alone:

> With a background load injected on the rig, the preflight gate keeps the
> coefficient of variation of a fixed CPU benchmark close to the quiet-machine
> value, and without the gate it does not.

It is weaker because it is measured on one machine with a synthetic noise
source, and the number it produces is about that machine, not about rigs in
general. `docs/known-misses.md` says so.

## 2. Verified environment

Checked on this machine, not assumed:

| Component | Local | Notes |
| --- | --- | --- |
| Go | 1.25.4 (homebrew) | `go.mod` pins 1.25 |
| PostgreSQL | 14.18 (homebrew) | running |
| Docker | 28.0.4 | daemon running, used for Linux agents |
| protoc | present | not used, see section 3 |
| Host | Apple M4, 10 cores, 16 GB | no NVIDIA GPU |

There is no physical embedded rig and no NVIDIA GPU here. Every rig in the
evidence is either this Mac or a Linux container on it, and any rig that
advertises hardware it does not have is marked `emulated: true` in its
capability descriptor, and that flag is copied into every run it produces. A
reader of a run artifact can always tell which kind of rig produced it.

## 3. Non-goals

Written down before the feature list, because the absences are decisions.

| Excluded | Why |
| --- | --- |
| Redis | The fence has to be durable and has to be issued on the same clock that decides expiry. Postgres does both in one statement. A second store would add a place for the two to disagree. ADR 0001. |
| gRPC and protobuf | The agent protocol is four calls. JSON over HTTP is enough to exercise every failure in this repo, and codegen would add a toolchain without adding a test. Revisit if the protocol grows streaming. |
| Kubernetes manifests | The control plane is stateless and would run fine on Kubernetes, but nothing here would test that it does. Rig agents are deliberately not pods: a rig is scarce hardware owned by a host process, not schedulable capacity. |
| Real GPU telemetry | No GPU on this host. The collector interface has an NVIDIA implementation that shells out to `nvidia-smi` when present, and it is exercised by a fixture, not by hardware. Recorded in known-misses. |
| Statistical regression detection | That is TraceLab's job. benchgrid reports raw samples and descriptive statistics, never a verdict on whether a change regressed. |
| Multi-tenant fair share | The scheduler ranks candidates by queue age and recent use of the rig. Weighted per-team quotas would have one tenant to test against. |

## 4. Architecture

```
  CI / bgctl                 POST /v1/experiments (ExperimentSpec)
       |
       v
  benchgrid server           stateless, any number of replicas
    api                      validates spec, stores it immutable with its sha256
    scheduler loop           claims a queued experiment with SKIP LOCKED,
       |                     hard-filters rigs by capability, ranks survivors
       v
  lease (Postgres)           one UPDATE grants the rig and increments its fence
       |                     expiry judged on the database clock
       v
  rig agent                  one per rig, a host process, not a pod
    fence store              highest token seen, fsynced before it is acted on
    state machine            READY LEASED PREFLIGHT RUNNING COLLECTING CLEANUP
    preflight                identity, load, memory, thermal, governor
    executor                 warmups, measured repetitions, timeout escalation
    spool                    completion buffered on disk until acknowledged
       |
       v
  artifact store             content addressed, a directory standing in for GCS
       |                     runs/<run_id>/run.json samples.jsonl manifest.json
       v
  TraceLab                   reads the run directory, owns every verdict
```

### The lease

```
UPDATE rigs
   SET fence = fence + 1, holder = $1, experiment_id = $2,
       expires_at = clock_timestamp() + $3
 WHERE id = $4
   AND (holder IS NULL OR expires_at < clock_timestamp())
RETURNING fence
```

One statement, so there is no window between checking and taking. Expiry is
compared against the database clock, so a scheduler with a skewed clock cannot
decide on its own that a lease has lapsed.

Renewal is conditioned on the fence, not on expiry:

```
UPDATE rigs SET expires_at = clock_timestamp() + $2
 WHERE id = $1 AND fence = $3
```

A holder whose lease lapsed but which nobody else took is still the holder,
because the fence never moved. Expiry is permission for someone else to take
over, not an eviction. That distinction is what lets a run survive a control
plane outage longer than its TTL.

### The fence at the rig

The agent keeps the highest token it has seen. A command carrying a lower token
is rejected. A command carrying a higher token first terminates and reaps every
process launched under a lower token, then persists the new high-water mark,
then proceeds. A command repeating the current token and experiment is answered
with the existing run's status rather than starting a second one.

The high-water mark is fsynced to disk before the agent acts on it. Without
that, an agent restart would forget it, and a frozen scheduler resuming after
the restart would be accepted.

## 5. Evidence plan

All of it produced by `cmd/evidence`, written to `evidence/results/<stamp>/`
with raw intervals beside every summary, and recomputed by
`script/validate_evidence.go` in CI, which also checks a sha256 manifest so a
hand-edited number fails the build.

| Run | What varies | What is published |
| --- | --- | --- |
| lease race | 64 workers, 20 rigs, conditional UPDATE versus a naive check-then-set | every holder interval on the database clock, double bookings per mode |
| fence | real scheduler and agent processes, schedulers frozen past TTL, fenced versus unfenced agent | every process interval per rig, overlapping pairs per mode |
| chaos | agents killed, schedulers killed and frozen, artifact upload failures | every experiment's terminal state, rigs left leased or quarantined |
| noise | quiet, loaded without gate, loaded with gate | every sample, CV per condition |

## 6. Run artifact contract

Owned here, consumed by TraceLab. Specified in `docs/run-artifact.md`.
