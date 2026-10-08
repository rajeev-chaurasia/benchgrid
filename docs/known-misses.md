# Known misses

Written adversarially: each entry is a way the published evidence is weaker
than it looks, or a way the system can still be wrong.

## Every rig is a process on one machine

No physical rig, no embedded board, and no GPU took part in anything in
`evidence/`. The rigs are agents on one Apple M4 host, as processes or as
Linux containers in Docker's Linux VM on that host, which proves the
scheduling, leasing, and fencing logic on both kernels, and proves nothing
about how a benchmark behaves on the hardware an emulated rig imitates. Every
run says `emulated` where that applies, and the host is recorded in
`env.json`.

## perf has never counted a real event here

The perf collector wraps each iteration in `perf stat` and turns its counts
into metrics, and it fails a run that asked for a counter perf could not
provide. Neither macOS nor Docker's Linux VM exposes hardware counters, so it
has only ever run against a stand-in perf that writes hand-written output in
perf's documented format. Running under perf also adds its own overhead to
every iteration's latency, which is why a spec with a collector hashes
differently from one without and is never compared with it.

## The CPU governor has never been set on real hardware

Neither macOS nor Docker's Linux VM exposes cpufreq, so no evidence run has
ever changed a governor. The logic is tested against a directory laid out as
the kernel lays out `/sys/devices/system/cpu`, including a read-only file
standing in for a kernel that refuses, which tests the agent's behaviour and
not the kernel's.

## The fence run aims its freezes

Schedulers stop themselves at the one point where a freeze turns into a stale
dispatch: after committing a lease, before sending it. The chaos run's freezes
and kills land at random and also produce stale dispatches, and its summary
counts them, so the README shows both the aimed and the unaided count. The
aimed run exists because the unaided rate is too low to measure the fence
against in a run of this length.

## An orphan's end time is an upper bound

When an agent dies mid-run, the next agent on the rig kills its benchmark and
records the interval it ran. If the benchmark had already ended by itself, the
true end is unknown, and the interval is recorded as ending when the new
agent started. That can only make an interval longer, and no new work could
start on the rig while it had no agent, so it cannot hide an overlap; it can
only make the published intervals pessimistic.

## A launch the agent never recorded quarantines the rig

The agent writes a marker before starting a benchmark and replaces it with the
process group id after. An agent killed between the two leaves a marker with
no id, and the next agent quarantines the rig rather than guess whether a
process is loose. That is safe and occasionally inconvenient.

## Process group ids are checked, not just trusted

A recorded group whose leader now has a different creation time is assumed
reused and left alone. That is only correct because POSIX systems do not
reuse a process id while a process group with that id still exists, so a
leader with a new creation time means the old group is gone. A system that
broke that rule would make the agent leave a stale group running.

## A multi-GPU rig is described as uniform

The rig advertises its first device's model, the smallest memory of any
device, and the count. A rig with two different GPUs can satisfy a spec that
asks for the larger model only if the first device listed is that model.

## Kernel isolation showed no benefit on these VMs

The isolation study was meant to show tuned nodes measuring more steadily
than default ones. On `n2d-standard-4` VMs it did not: with one thread per
core, both classes were already well under one percent variation, and the
largest effect in the data is one tuned VM several times noisier than its
peers, which no setting inside a VM can fix. A bare-metal rig with frequency
scaling, SMT and a busy system core is where isolation earns its keep, and
nothing here measured one. The calibration canary exists because of this.

## The canary was designed from one study, not evaluated by another

The rig noise canary and the scheduler's `max_rig_noise_cv` were built after
the isolation study exposed a noisy VM. The canaries are published for the
fleet that ran, and tests show noisy and unmeasured rigs are kept away from a
spec that asks, but no study has yet measured how much the gate's false alarm
rate falls when noisy rigs are excluded.

## Most gate pairs did not share a rig

Paired runs share an affinity key and prefer the same rig, but with three
nodes and several comparisons in flight the preferred rig was usually busy,
and a pair split across two machines adds machine to machine variation to
every difference the gate sees. Waiting for the preferred rig would trade
throughput for that.

## The GPU node is in another region

GPUs were out of stock in every us-west1 zone when the evidence was taken, so
the GPU node ran in us-central1 against the control plane in us-west1 over
the VPC. That adds latency to the agent's control calls, not to anything it
measures, and its run count is small.

## PXE is proven on a simulated machine

`deploy/pxe` boots a diskless rig over the network in CI: PXE firmware,
DHCP, TFTP and HTTP boot through iPXE are all real, and the rig must register
and complete an experiment for the job to pass. The machine is QEMU and the
network is QEMU's user mode network, so nothing here has PXE booted physical
hardware, and the GCP fleet itself is provisioned from images by a startup
script, because GCE cannot network boot.

## The sensor in the loop is simulated

`avbench --loop` drives a fixed rate cycle from an absolute timer and replays
a recording of frames made before the loop starts, which is the timing
structure of a perception stage fed by a sensor. No real sensor, bus or
interrupt is involved, so its jitter is the scheduler's and the timer's, not a
device's.

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
