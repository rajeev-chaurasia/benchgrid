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

## Running rigs on Kubernetes

The control plane runs on GKE (`deploy/gcp`), and `deploy/k8s` holds a
generic manifest that CI checks decodes strictly and matches the binary's
flags. Rig agents are deliberately not pods: a rig is scarce hardware owned by
a host process, and scheduling it as capacity is the mistake this design
avoids. `deploy/systemd` and `deploy/gcp/rig-startup.sh` are how a rig runs
its agent.

## Real GPU telemetry

There is no GPU on the development machine. The probe parses `nvidia-smi`
output for every device when present, and that parser is exercised by
hand-written fixtures, not by hardware. `known-misses.md` says so.

## Analysis beyond loading

`benchgrid-bq` loads every sealed run into BigQuery, partitioned and
clustered for the queries a performance dashboard makes. Dashboards
themselves, and any analysis past a gate decision, belong to whatever reads
those tables.

## Choosing a baseline from history

`benchgrid-gate` decides between two builds it runs itself, in pairs. It does
not choose a baseline from past runs, track trends, or compare across weeks:
that needs a history and a policy for what counts as healthy, which belong to
an analysis system reading the BigQuery tables.

## Fair share between teams

Candidates are ranked by how long a rig has been idle. Weighted per-team
quotas would have one tenant to test against here.

## Automatic return from quarantine

A quarantined rig stays out until an operator calls `unquarantine`. Whatever
quarantined it has not been shown to be gone, and a rig that cycles in and
out of service produces numbers nobody should trust.
