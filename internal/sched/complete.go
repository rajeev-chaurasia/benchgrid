package sched

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/lease"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

var (
	ErrUnknownAttempt = errors.New("no such attempt on that rig and fence")
	ErrConflict       = errors.New("attempt already finished with a different status")
)

// Retryable decides whether a finished attempt earns another. An environment
// that did not hold, a preemption, or an artifact that could not be fetched
// says nothing about the code, so the experiment is tried again. A benchmark
// that exits nonzero, crashes, or times out is a property of the code and
// would do it again, so it is final.
func Retryable(status, reason string) bool {
	switch {
	case status == "INVALID":
		return true
	case status == "FAILED" && strings.HasPrefix(reason, "artifact:"):
		return true
	}
	return false
}

// Complete records an agent's report of a finished attempt. It is idempotent:
// the agent repeats it until acknowledged, and a repeat of an identical report
// succeeds without changing anything.
//
// A late report from an attempt that was already requeued is still accepted
// if no newer attempt has started, because it is a genuine measurement of the
// same spec and running it again would only measure it twice. Once a newer
// attempt exists, the late one is recorded on its attempt row and closes
// nothing.
func Complete(ctx context.Context, db *pgxpool.Pool, experimentID string, attempt int, c wire.Completion) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var prevStatus string
	var finished bool
	err = tx.QueryRow(ctx, `
		SELECT status, finished_at IS NOT NULL FROM attempts
		 WHERE experiment_id = $1 AND attempt = $2 AND rig_id = $3 AND fence = $4
		 FOR UPDATE`, experimentID, attempt, c.RigID, c.Fence).Scan(&prevStatus, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnknownAttempt
	}
	if err != nil {
		return err
	}
	if finished && prevStatus != "ABANDONED" {
		if prevStatus == c.Status {
			return nil
		}
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET status = $3, status_reason = $4, finished_at = clock_timestamp()
		 WHERE experiment_id = $1 AND attempt = $2`, experimentID, attempt, c.Status, c.StatusReason); err != nil {
		return err
	}

	retry := Retryable(c.Status, c.StatusReason)
	if retry {
		_, err = tx.Exec(ctx, `
			UPDATE experiments
			   SET state = CASE WHEN attempt < max_attempts THEN 'QUEUED' ELSE 'FAILED' END,
			       status_reason = CASE WHEN attempt < max_attempts THEN $3 ELSE 'attempts_exhausted:' || $3 END,
			       finished_at = CASE WHEN attempt < max_attempts THEN NULL ELSE clock_timestamp() END,
			       rig_id = NULL, fence = NULL, updated_at = clock_timestamp()
			 WHERE id = $1 AND attempt = $2 AND state IN ('RUNNING', 'QUEUED')`,
			experimentID, attempt, c.Status+":"+c.StatusReason)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE experiments
			   SET state = $3, status_reason = $4, finished_at = clock_timestamp(),
			       updated_at = clock_timestamp()
			 WHERE id = $1 AND attempt = $2 AND state IN ('RUNNING', 'QUEUED')`,
			experimentID, attempt, c.Status, c.StatusReason)
	}
	if err != nil {
		return err
	}
	if _, err := lease.Release(ctx, tx, c.RigID, c.Fence); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
