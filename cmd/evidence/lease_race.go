package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
	"github.com/rajeev-chaurasia/benchgrid/internal/lease"
)

const (
	raceRigs     = 20
	raceWorkers  = 64
	raceAttempts = 50000
	// One grant in ten is never released and must expire instead, so the race
	// covers takeover after expiry and not only release and reacquire.
	raceAbandon = 0.1
)

type acquirer func(ctx context.Context, db *pgxpool.Pool, rig, holder string, ttl time.Duration) (lease.Grant, bool, error)

func conditional(ctx context.Context, db *pgxpool.Pool, rig, holder string, ttl time.Duration) (lease.Grant, bool, error) {
	return lease.Acquire(ctx, db, rig, holder, "race", 1, ttl)
}

// naive is the check-then-set every hand-rolled lock starts as: read whether
// the rig is free, then take it with an unconditional write. It lives in the
// harness, not in internal/lease, so nothing in the product can call it.
func naive(ctx context.Context, db *pgxpool.Pool, rig, holder string, ttl time.Duration) (lease.Grant, bool, error) {
	var free bool
	if err := db.QueryRow(ctx, `SELECT holder IS NULL OR expires_at < clock_timestamp() FROM rigs WHERE id = $1`, rig).Scan(&free); err != nil {
		return lease.Grant{}, false, err
	}
	if !free {
		return lease.Grant{}, false, nil
	}
	g := lease.Grant{RigID: rig}
	err := db.QueryRow(ctx, `
		UPDATE rigs SET fence = fence + 1, holder = $2, experiment_id = 'race', attempt = 1,
		       expires_at = clock_timestamp() + make_interval(secs => $3)
		 WHERE id = $1
		RETURNING fence, clock_timestamp(), expires_at`, rig, holder, ttl.Seconds()).Scan(&g.Fence, &g.AcquiredAt, &g.ExpiresAt)
	return g, err == nil, err
}

func (h *harness) leaseRace(ctx context.Context) error {
	dir := filepath.Join(h.out, "lease_race")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var summaries []evidence.RaceSummary
	for _, mode := range []struct {
		name string
		fn   acquirer
	}{{"conditional", conditional}, {"naive", naive}} {
		s, grants, ops, err := h.race(ctx, mode.name, mode.fn)
		if err != nil {
			return err
		}
		if err := evidence.WriteJSONLGz(filepath.Join(dir, mode.name+".grants.jsonl.gz"), grants); err != nil {
			return err
		}
		if err := evidence.WriteJSONLGz(filepath.Join(dir, mode.name+".ops.jsonl.gz"), ops); err != nil {
			return err
		}
		summaries = append(summaries, s)
	}
	return evidence.WriteJSON(filepath.Join(dir, "summary.json"), summaries)
}

func (h *harness) race(ctx context.Context, mode string, acquire acquirer) (evidence.RaceSummary, []evidence.Grant, []evidence.Op, error) {
	db, _, err := h.freshDB(ctx, "benchgrid_evidence_race")
	if err != nil {
		return evidence.RaceSummary{}, nil, nil, err
	}
	defer db.Close()
	for i := 0; i < raceRigs; i++ {
		if _, err := db.Exec(ctx, `INSERT INTO rigs (id, descriptor) VALUES ($1, '{}')`, rigName(i)); err != nil {
			return evidence.RaceSummary{}, nil, nil, err
		}
	}

	epoch := time.Now()
	var next atomic.Int64
	var mu sync.Mutex
	var grants []evidence.Grant
	var ops []evidence.Op
	var errs atomic.Int64
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < raceWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 0x62656e6368))
			<-gate
			for {
				// The holder name must be unique per attempt. A name shared by two
				// grants would make their overlap look like one holder's and hide
				// exactly the double booking being counted.
				n := next.Add(1)
				if n > raceAttempts {
					return
				}
				rig := rigName(rng.IntN(raceRigs))
				holder := fmt.Sprintf("op-%d", n)
				ttl := time.Duration(5+rng.IntN(15)) * time.Millisecond
				t0 := time.Since(epoch).Nanoseconds()
				g, ok, err := acquire(ctx, db, rig, holder, ttl)
				t1 := time.Since(epoch).Nanoseconds()
				if err != nil {
					errs.Add(1)
					continue
				}
				op := evidence.Op{Worker: w, StartNS: t0, EndNS: t1, Granted: ok}
				if !ok {
					mu.Lock()
					ops = append(ops, op)
					mu.Unlock()
					continue
				}
				gr := evidence.Grant{Rig: rig, Holder: holder, Fence: g.Fence, StartUS: g.AcquiredAt.UnixMicro(), EndUS: g.ExpiresAt.UnixMicro(), Ended: "expired"}
				if rng.Float64() >= raceAbandon {
					time.Sleep(time.Duration(rng.IntN(3000)) * time.Microsecond)
					at, released, err := lease.ReleaseAt(ctx, db, rig, g.Fence)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						errs.Add(1)
					}
					// A release after expiry that still succeeds means nobody
					// took the rig in between, so the belief really did last
					// until the release.
					if released {
						gr.EndUS, gr.Ended = at.UnixMicro(), "released"
					}
				}
				mu.Lock()
				ops = append(ops, op)
				grants = append(grants, gr)
				mu.Unlock()
			}
		}(w)
	}
	close(gate)
	wg.Wait()

	s := evidence.SummarizeRace(mode, grants, ops)
	s.Rigs, s.Workers, s.Errors = raceRigs, raceWorkers, int(errs.Load())
	return s, grants, ops, nil
}

func rigName(i int) string { return "rig-" + strconv.Itoa(i) }
