# Known misses

Written adversarially: each entry is a way the published evidence is weaker
than it looks, or a way the system can still be wrong.

## Every rig is a process on one machine

No physical rig, no embedded board, and no GPU took part in anything in
`evidence/`. The rigs are agents on one Apple M4 host, which proves the
scheduling, leasing, and fencing logic, and proves nothing about how a
benchmark behaves on the hardware an emulated rig imitates. Every run says
`emulated` where that applies, and the host is recorded in `env.json`.

## The fence run aims its freezes

Schedulers stop themselves at the one point where a freeze turns into a stale
dispatch: after committing a lease, before sending it. Freezes at random
points would land there rarely and the run would mostly measure nothing. The
cost is that the run shows the fence handles the dangerous case, not how
often that case arises unaided.

## Intervals of work interrupted by an agent's death are not recorded

An agent killed mid-run never writes the end of the interval it was in. Its
benchmark keeps running, in its own process group, until the next agent on
that rig starts and kills it. No new work can start on the rig in that gap,
because there is no agent to accept it, so no overlap can hide there, but the
gap itself is invisible in the published intervals.

## Process group ids can be reused

A restarted agent kills every process group its predecessor recorded and did
not see finish. If the operating system reused one of those ids for an
unrelated process group in between, that group is killed. Recording each
group's start time alongside its id would close this.

## The control plane can lose the window between start and record

A benchmark process is started, and then its group id is written to disk. An
agent killed between those two steps leaves a group nobody recorded.

## A multi-GPU rig is advertised by its first GPU

`nvidia-smi` is asked about every device and only the first line is used.

## The noise run is about this machine

The second claim is measured with a synthetic, seeded noise source on one
laptop class CPU with no control over frequency scaling. It says the gate
behaves as designed against bursty load. It does not say what variation any
real rig will show, and the numbers should not be quoted as if it did.

## The lease race measures intervals at microsecond resolution

Postgres timestamps are microseconds. Two grants a few hundred nanoseconds
apart on one rig would be recorded at the same instant and, being half open,
counted as touching rather than overlapping. The conditional statement makes
such a pair impossible by construction, but the measurement alone could not
tell.
