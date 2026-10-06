// Package sched places queued experiments on rigs. Placement is two stages:
// a hard filter on capability, then a ranking among the rigs that survive.
// Ranking never sees a rig the filter rejected, so a fast decision can never
// be a wrong one.
package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/lease"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

type Config struct {
	ID       string
	LeaseTTL time.Duration
	Tick     time.Duration
	// HeartbeatStale is how long a rig may go silent before it is not offered
	// new work. Its existing lease is governed by the TTL, not by this.
	HeartbeatStale time.Duration
	Batch          int
	// FreezeBeforeDispatch is fault injection for the evidence harness only.
	// With this probability the scheduler stops its own process with SIGSTOP
	// after committing a lease and before dispatching it, which is the one
	// window where a frozen scheduler becomes a stale one. Freezing at random
	// points would land there too rarely to measure anything.
	FreezeBeforeDispatch float64
	Logger               *slog.Logger
}

type Scheduler struct {
	cfg    Config
	db     *pgxpool.Pool
	client *http.Client
	log    *slog.Logger
	m      *Metrics
}

type Metrics struct {
	Placements  prometheus.Counter
	Dispatches  *prometheus.CounterVec
	Unplaceable prometheus.Counter
	Requeues    *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Placements: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "benchgrid_placements_total", Help: "Experiments granted a rig lease."}),
		Dispatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "benchgrid_dispatches_total", Help: "Dispatches sent to agents, by result."}, []string{"result"}),
		Unplaceable: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "benchgrid_unplaceable_total", Help: "Scheduling passes that found no eligible free rig for an experiment."}),
		Requeues: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "benchgrid_requeues_total", Help: "Attempts abandoned and requeued, by reason."}, []string{"reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.Placements, m.Dispatches, m.Unplaceable, m.Requeues)
	}
	return m
}

func New(cfg Config, db *pgxpool.Pool, m *Metrics) *Scheduler {
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = 5 * time.Second
	}
	if cfg.Tick == 0 {
		cfg.Tick = 200 * time.Millisecond
	}
	if cfg.HeartbeatStale == 0 {
		cfg.HeartbeatStale = 3 * time.Second
	}
	if cfg.Batch == 0 {
		cfg.Batch = 16
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if m == nil {
		m = NewMetrics(nil)
	}
	return &Scheduler{cfg: cfg, db: db, m: m, log: cfg.Logger.With("scheduler", cfg.ID),
		client: &http.Client{Timeout: 5 * time.Second}}
}

type candidate struct {
	rig          capability.Rig
	lastReleased time.Time
}

type placement struct {
	ExperimentID string
	Attempt      int
	Spec         spec.Spec
	Rig          capability.Rig
	Grant        lease.Grant
}

func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		if err := s.Reap(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("reap failed", "err", err)
		}
		placed, err := s.Place(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("place failed", "err", err)
		}
		for _, p := range placed {
			s.maybeFreeze()
			s.dispatch(ctx, p)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Scheduler) maybeFreeze() {
	if s.cfg.FreezeBeforeDispatch > 0 && rand.Float64() < s.cfg.FreezeBeforeDispatch {
		s.log.Warn("freezing before dispatch (fault injection)")
		syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
}

// Place claims queued experiments and leases a rig for each in one
// transaction. The attempt counter is incremented only when a rig was granted,
// so a pass that finds nothing free consumes nothing.
func (s *Scheduler) Place(ctx context.Context) ([]placement, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, spec, attempt FROM experiments
		 WHERE state = 'QUEUED'
		 ORDER BY submitted_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, s.cfg.Batch)
	if err != nil {
		return nil, err
	}
	type queued struct {
		id      string
		spec    spec.Spec
		attempt int
	}
	var queue []queued
	for rows.Next() {
		var q queued
		var raw []byte
		if err := rows.Scan(&q.id, &raw, &q.attempt); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(raw, &q.spec); err != nil {
			rows.Close()
			return nil, err
		}
		queue = append(queue, q)
	}
	rows.Close()
	if len(queue) == 0 {
		return nil, nil
	}

	free, err := s.freeRigs(ctx, tx)
	if err != nil {
		return nil, err
	}

	var placed []placement
	for _, q := range queue {
		var eligible []candidate
		for _, c := range free {
			if len(capability.Match(q.spec.Requirements, q.spec.Environment, c.rig)) == 0 {
				eligible = append(eligible, c)
			}
		}
		if len(eligible) == 0 {
			s.m.Unplaceable.Inc()
			continue
		}
		rank(eligible)
		attempt := q.attempt + 1
		for _, c := range eligible {
			g, ok, err := lease.Acquire(ctx, tx, c.rig.RigID, s.cfg.ID, q.id, attempt, s.cfg.LeaseTTL)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx, `
				UPDATE experiments
				   SET state = 'RUNNING', attempt = $2, rig_id = $3, fence = $4,
				       status_reason = '', updated_at = clock_timestamp()
				 WHERE id = $1`, q.id, attempt, c.rig.RigID, g.Fence); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO attempts (experiment_id, attempt, rig_id, fence, scheduler, leased_at)
				VALUES ($1, $2, $3, $4, $5, $6)`, q.id, attempt, c.rig.RigID, g.Fence, s.cfg.ID, g.AcquiredAt); err != nil {
				return nil, err
			}
			placed = append(placed, placement{ExperimentID: q.id, Attempt: attempt, Spec: q.spec, Rig: c.rig, Grant: g})
			free = without(free, c.rig.RigID)
			break
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.m.Placements.Add(float64(len(placed)))
	return placed, nil
}

func (s *Scheduler) freeRigs(ctx context.Context, tx pgx.Tx) ([]candidate, error) {
	rows, err := tx.Query(ctx, `
		SELECT descriptor, COALESCE(last_released_at, 'epoch'::timestamptz)
		  FROM rigs
		 WHERE agent_state = 'READY'
		   AND last_heartbeat > clock_timestamp() - make_interval(secs => $1)
		   AND (holder IS NULL OR expires_at < clock_timestamp())`, s.cfg.HeartbeatStale.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var raw []byte
		var c candidate
		if err := rows.Scan(&raw, &c.lastReleased); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &c.rig); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rank prefers the rig that has been idle longest. That spreads work across
// equivalent rigs and gives the one that just finished a heavy run the most
// time to cool before it is measured on again. The id breaks ties so the
// order is deterministic.
func rank(cs []candidate) {
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].lastReleased.Equal(cs[j].lastReleased) {
			return cs[i].lastReleased.Before(cs[j].lastReleased)
		}
		return cs[i].rig.RigID < cs[j].rig.RigID
	})
}

func without(cs []candidate, id string) []candidate {
	out := cs[:0:0]
	for _, c := range cs {
		if c.rig.RigID != id {
			out = append(out, c)
		}
	}
	return out
}

// dispatch sends the lease to the agent. An explicit refusal frees the rig and
// requeues the experiment at once. A network error does neither: the agent
// may have accepted, and if it did not, its heartbeats will not renew the
// lease and the reaper requeues the experiment when the lease lapses. Guessing
// here would either strand a rig or run an attempt twice.
func (s *Scheduler) dispatch(ctx context.Context, p placement) {
	d := wire.Dispatch{
		ExperimentID: p.ExperimentID, Attempt: p.Attempt, Fence: p.Grant.Fence,
		LeaseAcquired: p.Grant.AcquiredAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Spec:          p.Spec,
	}
	reply, code, err := s.send(ctx, p.Rig.Endpoint, d)
	switch {
	case err != nil:
		s.m.Dispatches.WithLabelValues("unreachable").Inc()
		s.log.Warn("dispatch unreachable", "experiment", p.ExperimentID, "rig", p.Rig.RigID, "err", err)
	case reply.Accepted:
		s.m.Dispatches.WithLabelValues("accepted").Inc()
		s.db.Exec(ctx, `UPDATE attempts SET dispatched_at = clock_timestamp(), status = 'DISPATCHED'
			WHERE experiment_id = $1 AND attempt = $2`, p.ExperimentID, p.Attempt)
	default:
		s.m.Dispatches.WithLabelValues(reply.Reason).Inc()
		s.log.Warn("dispatch refused", "experiment", p.ExperimentID, "rig", p.Rig.RigID,
			"fence", p.Grant.Fence, "reason", reply.Reason, "status", code)
		if err := s.abandon(ctx, p.ExperimentID, p.Attempt, p.Rig.RigID, p.Grant.Fence, "refused:"+reply.Reason); err != nil {
			s.log.Warn("abandon failed", "err", err)
		}
	}
}

func (s *Scheduler) send(ctx context.Context, endpoint string, d wire.Dispatch) (wire.DispatchReply, int, error) {
	var reply wire.DispatchReply
	b, _ := json.Marshal(d)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/runs", bytes.NewReader(b))
	if err != nil {
		return reply, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return reply, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err := json.Unmarshal(body, &reply); err != nil {
		return reply, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return reply, resp.StatusCode, nil
}

// abandon gives up one attempt: the rig is released if this attempt still
// holds it, and the experiment goes back to the queue if this attempt is still
// its current one. Both conditions matter, because by the time a refusal
// arrives either may already have moved on.
func (s *Scheduler) abandon(ctx context.Context, experimentID string, attempt int, rigID string, fence int64, reason string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lease.Release(ctx, tx, rigID, fence); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE attempts SET status = 'ABANDONED', status_reason = $3, finished_at = clock_timestamp()
		WHERE experiment_id = $1 AND attempt = $2 AND finished_at IS NULL`, experimentID, attempt, reason); err != nil {
		return err
	}
	if err := requeue(ctx, tx, experimentID, attempt, reason); err != nil {
		return err
	}
	s.m.Requeues.WithLabelValues(strings.SplitN(reason, ":", 2)[0]).Inc()
	return tx.Commit(ctx)
}

// requeue returns an experiment to the queue, or fails it when it has used
// every attempt. It does nothing if the experiment has already moved past the
// given attempt.
func requeue(ctx context.Context, tx pgx.Tx, experimentID string, attempt int, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE experiments
		   SET state = CASE WHEN attempt < max_attempts THEN 'QUEUED' ELSE 'FAILED' END,
		       status_reason = CASE WHEN attempt < max_attempts THEN $3 ELSE 'attempts_exhausted:' || $3 END,
		       finished_at = CASE WHEN attempt < max_attempts THEN NULL ELSE clock_timestamp() END,
		       rig_id = NULL, fence = NULL, updated_at = clock_timestamp()
		 WHERE id = $1 AND attempt = $2 AND state = 'RUNNING'`, experimentID, attempt, reason)
	return err
}

// Reap requeues every running experiment whose lease is no longer live: either
// someone else now holds its rig, or nobody has renewed it within the TTL.
// Agents renew through their heartbeats, so a lapsed lease means no agent is
// running the attempt, or none can say so. If one still is and reports later,
// its result is accepted on its own merits; see Complete in the server.
func (s *Scheduler) Reap(ctx context.Context) error {
	rows, err := s.db.Query(ctx, `
		SELECT e.id, e.attempt, e.rig_id, e.fence,
		       CASE WHEN r.fence <> e.fence THEN 'lease_taken' ELSE 'lease_expired' END
		  FROM experiments e JOIN rigs r ON r.id = e.rig_id
		 WHERE e.state = 'RUNNING'
		   AND (r.fence <> e.fence OR r.holder IS NULL OR r.expires_at < clock_timestamp())`)
	if err != nil {
		return err
	}
	type lost struct {
		id, rig, reason string
		attempt         int
		fence           int64
	}
	var all []lost
	for rows.Next() {
		var l lost
		if err := rows.Scan(&l.id, &l.attempt, &l.rig, &l.fence, &l.reason); err != nil {
			rows.Close()
			return err
		}
		all = append(all, l)
	}
	rows.Close()
	for _, l := range all {
		if err := s.abandon(ctx, l.id, l.attempt, l.rig, l.fence, l.reason); err != nil {
			return err
		}
		s.log.Info("requeued", "experiment", l.id, "attempt", l.attempt, "reason", l.reason)
	}
	return nil
}
