# 0003. The rig enforces the fence, and remembers it

## Decision

Every dispatch carries the lease's fence. The agent keeps the highest fence
it has seen, fsynced to disk before it acts on a new one. A lower fence is
refused. A higher one first terminates and reaps everything running under a
lower fence, then proceeds.

## Why

The lease table can only say who should own a rig. It cannot stop a scheduler
that was frozen past its lease from waking up and sending a command anyway,
because that scheduler does not know it was frozen. The only place that sees
every command is the rig, so the rig is where ownership is enforced. This is
the fencing token pattern, and the lease would be incomplete without it.

The high-water mark is persisted before use because an agent restart would
otherwise reset it, and the stale scheduler would then be accepted by the
fresh agent. A corrupt mark stops the agent from starting rather than being
guessed at.

## How it is shown to work

`evidence/` runs real scheduler processes that stop themselves with `SIGSTOP`
between leasing a rig and dispatching to it, against fenced agents and
against agents started with `-unfenced-negative-control`. Overlap is computed
from every process interval the agents recorded, on the host's monotonic
clock. The validator fails the build if the fenced run overlaps or if the
control ever stops overlapping.
