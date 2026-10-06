# 0001. Leases live in Postgres, not Redis

## Decision

A rig lease is three columns on the rig's row: `holder`, `expires_at`, and
`fence`. It is granted by one conditional `UPDATE` that also increments the
fence, and expiry is judged by `clock_timestamp()` in the same statement.

## Why

The fence has to be monotonic for the life of the rig, through restarts of
everything. A Redis key with a TTL gives expiry but not that: an `INCR`
counter survives only as long as persistence is configured to keep it, and it
lives in a different store from the queue it has to agree with. Here the
fence, the expiry, the experiment claim, and the attempt counter all change
in one transaction, so there is no state in which they disagree.

Expiry is compared against the database clock, so no scheduler decides with
its own clock that someone else's lease has run out. `now()` was rejected for
the same reason in a smaller form: it is the start of the enclosing
transaction, which inside a scheduling pass can already be in the past.

## What it costs

Every grant, renewal, and release is a database write. At the scale measured
in `evidence/` (twenty rigs, sixty-four contending workers) that is nowhere
near a limit, and the lease race publishes its own throughput. A fleet large
enough for this to matter would also be large enough to shard rigs across
databases, which nothing here attempts.

## What a TTL alone would not have done

A TTL frees a lease whose holder died. It does nothing about a holder that
paused and woke up still believing it held the rig. That is ADR 0003.
