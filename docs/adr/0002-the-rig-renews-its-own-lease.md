# 0002. The rig renews its own lease

## Decision

Leases are renewed by agent heartbeats, conditioned on the fence the agent is
actually running. The scheduler that placed an attempt never renews it.

## Why

If the scheduler renewed, a scheduler crash would expire every lease it held
and requeue work that was still running fine. If nobody renewed, the TTL would
have to exceed the longest benchmark. The only party that knows whether an
attempt is still running is the rig, so the rig vouches for it.

Renewal is conditioned on the fence and not on expiry. A lease that lapsed
during a control plane outage but that nobody took over is still the agent's,
because the fence never moved, so the first heartbeat after the outage
resumes it rather than losing a run that never stopped.

## Consequence for the reaper

A lapsed lease alone no longer proves an attempt is dead: after an outage
every lease has lapsed. The reaper requeues only when someone else holds the
rig, when the rig has heartbeated since expiry without renewing, or when the
rig has been silent for the staleness window. It also refuses to act for a
settle period after it starts or wakes from a pause, because a scheduler that
was frozen sees every lease as expired and every heartbeat as old.
