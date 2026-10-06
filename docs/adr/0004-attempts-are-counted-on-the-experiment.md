# 0004. Attempts are counted on the experiment, not by fence

## Decision

Each experiment row has an attempt counter, incremented in the same
transaction that leases a rig for it. Run artifacts live at
`runs/<experiment>/attempt-<n>/`. The fence is recorded in the run for audit
and plays no part in ordering attempts.

## Why

The first draft of the contract used the fence as the attempt identity. That
is wrong, and the reason is worth keeping: the fence is counted per rig. A
retry on another rig can carry a smaller fence than the attempt it replaced,
and two attempts on two rigs can carry the same fence, so "highest fence
wins" would pick the stale attempt and two attempts could write into one
directory.

## Consequence

An attempt superseded on one rig can still seal after its replacement began
on another, since the old rig was never told to stop. Both are genuine
measurements of the same spec. The control plane accepts the late result if
no newer attempt has started and records it without effect if one has.
