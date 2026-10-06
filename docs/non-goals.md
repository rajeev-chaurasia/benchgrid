# Non-goals

Each of these is a real feature of a real benchmark platform, and each is
deliberately absent, so the absences read as decisions.

## Redis

The fence has to be durable and issued on the same clock that decides
expiry, and it has to change in the same transaction as the experiment it
belongs to. Postgres does all three in one statement. ADR 0001.

## gRPC and protobuf

The agent protocol is one command and three reports. JSON over HTTP
exercises every failure this repository tests, and code generation would add
a toolchain without adding a test. If the protocol grows streaming, for live
telemetry during a run, this should be revisited.

## Kubernetes manifests

The control plane is stateless and would run on Kubernetes unchanged, but
nothing here would test that it does, so no manifest is shipped. Rig agents
are deliberately not pods: a rig is scarce hardware owned by a host process,
and scheduling it as capacity is the mistake this design avoids.

## Real GPU telemetry

There is no GPU on the development machine. The probe parses `nvidia-smi`
output when present, and that parser is exercised by a hand-written fixture,
not by hardware. `known-misses.md` says so.

## Deciding whether a change regressed

benchgrid publishes raw samples and descriptive statistics, never a verdict.
Choosing a baseline, testing significance, and gating a merge on the result
belong to whatever reads the run artifacts.

## Fair share between teams

Candidates are ranked by how long a rig has been idle. Weighted per-team
quotas would have one tenant to test against here.

## Automatic return from quarantine

A quarantined rig stays out until an operator calls `unquarantine`. Whatever
quarantined it has not been shown to be gone, and a rig that cycles in and
out of service produces numbers nobody should trust.
