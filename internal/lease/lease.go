// Package lease grants exclusive, expiring ownership of a rig, and hands out a
// fence with every grant so the rig itself can refuse a holder that lost
// ownership without noticing.
//
// Every comparison against time uses clock_timestamp(), the database clock at
// the moment the statement runs. now() would be the start of the enclosing
// transaction, which inside a long scheduling transaction can be far enough in
// the past to grant a lease that has already expired. And no comparison uses
// the caller's clock, so a scheduler with a skewed clock cannot decide on its
// own that someone else's lease has lapsed.
package lease

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Grant struct {
	RigID      string
	Fence      int64
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// Acquire takes the rig if nobody holds it or the holder's lease has expired,
// in one statement, so there is no window between seeing the rig free and
// taking it. ok is false when the rig is held. The fence is incremented in the
// same statement, so no two grants on one rig can ever share a fence.
func Acquire(ctx context.Context, db DB, rigID, holder, experimentID string, attempt int, ttl time.Duration) (Grant, bool, error) {
	g := Grant{RigID: rigID}
	err := db.QueryRow(ctx, `
		UPDATE rigs
		   SET fence = fence + 1,
		       holder = $2,
		       experiment_id = $3,
		       attempt = $4,
		       expires_at = clock_timestamp() + make_interval(secs => $5)
		 WHERE id = $1
		   AND agent_state = 'READY'
		   AND (holder IS NULL OR expires_at < clock_timestamp())
		RETURNING fence, clock_timestamp(), expires_at`,
		rigID, holder, experimentID, attempt, ttl.Seconds(),
	).Scan(&g.Fence, &g.AcquiredAt, &g.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, err
	}
	return g, true, nil
}

// Renew extends a lease, conditioned on the fence and deliberately not on
// expiry. A holder whose lease lapsed but which nobody else took is still the
// holder, because the fence never moved. Expiry is permission for someone else
// to take over, not an eviction, and that is what lets a run outlive a control
// plane outage longer than its TTL.
func Renew(ctx context.Context, db DB, rigID string, fence int64, ttl time.Duration) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE rigs
		   SET expires_at = clock_timestamp() + make_interval(secs => $3)
		 WHERE id = $1 AND fence = $2 AND holder IS NOT NULL`,
		rigID, fence, ttl.Seconds())
	return tag.RowsAffected() == 1, err
}

// Release frees the rig only for the holder of the current fence, so a late
// completion from a superseded attempt cannot free a rig that someone else now
// holds.
func Release(ctx context.Context, db DB, rigID string, fence int64) (bool, error) {
	_, ok, err := ReleaseAt(ctx, db, rigID, fence)
	return ok, err
}

// ReleaseAt is Release that also reports when, on the database clock, which
// is what the lease race evidence measures holder intervals against.
func ReleaseAt(ctx context.Context, db DB, rigID string, fence int64) (time.Time, bool, error) {
	var at time.Time
	err := db.QueryRow(ctx, `
		UPDATE rigs
		   SET holder = NULL, experiment_id = NULL, attempt = NULL,
		       expires_at = NULL, last_released_at = clock_timestamp()
		 WHERE id = $1 AND fence = $2 AND holder IS NOT NULL
		RETURNING last_released_at`,
		rigID, fence).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return at, err == nil, err
}
