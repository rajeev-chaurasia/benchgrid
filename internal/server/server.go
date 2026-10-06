// Package server is the control plane's HTTP surface. It holds no state of
// its own: every replica reads and writes the same database, so any number of
// them can run, and any of them can stop.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/lease"
	"github.com/rajeev-chaurasia/benchgrid/internal/sched"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

type Server struct {
	DB       *pgxpool.Pool
	Store    *artifact.FSStore
	LeaseTTL time.Duration
	Log      *slog.Logger
	Registry *prometheus.Registry

	heartbeats  prometheus.Counter
	completions *prometheus.CounterVec
}

type Submission struct {
	Spec           json.RawMessage `json:"spec"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	MaxAttempts    int             `json:"max_attempts,omitempty"`
}

type Experiment struct {
	ID           string    `json:"id"`
	State        string    `json:"state"`
	StatusReason string    `json:"status_reason"`
	Attempt      int       `json:"attempt"`
	MaxAttempts  int       `json:"max_attempts"`
	SpecSHA256   string    `json:"spec_sha256"`
	RigID        *string   `json:"rig_id"`
	Fence        *int64    `json:"fence"`
	SubmittedAt  time.Time `json:"submitted_at"`
	Attempts     []Attempt `json:"attempts,omitempty"`
}

type Attempt struct {
	Attempt      int        `json:"attempt"`
	RigID        string     `json:"rig_id"`
	Fence        int64      `json:"fence"`
	Scheduler    string     `json:"scheduler"`
	Status       string     `json:"status"`
	StatusReason string     `json:"status_reason"`
	LeasedAt     time.Time  `json:"leased_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.LeaseTTL == 0 {
		s.LeaseTTL = 5 * time.Second
	}
	s.heartbeats = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "benchgrid_heartbeats_total", Help: "Agent heartbeats received."})
	s.completions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "benchgrid_completions_total", Help: "Attempt completions reported, by status."}, []string{"status"})
	mux := http.NewServeMux()
	if s.Registry != nil {
		s.Registry.MustRegister(s.heartbeats, s.completions, &stateCollector{db: s.DB})
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.Registry, promhttp.HandlerOpts{}))
	}

	mux.HandleFunc("POST /v1/experiments", s.submit)
	mux.HandleFunc("GET /v1/experiments/{id}", s.getExperiment)
	mux.HandleFunc("POST /v1/experiments/{id}/attempts/{attempt}/complete", s.complete)
	mux.HandleFunc("POST /v1/rigs/{id}/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /v1/rigs", s.listRigs)
	mux.HandleFunc("POST /v1/blobs", s.putBlob)
	mux.HandleFunc("GET /v1/blobs/{digest}", s.getBlob)
	mux.HandleFunc("PUT /v1/artifacts/runs/{run}/{attempt}/{file}", s.putArtifact)
	mux.HandleFunc("POST /v1/artifacts/runs/{run}/{attempt}/seal", s.seal)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ping(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	return mux
}

func newID() string {
	var b [10]byte
	rand.Read(b[:])
	return fmt.Sprintf("exp_%012x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var sub Submission
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sub); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sp, err := spec.Parse(sub.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	digest, err := sp.SHA256()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sub.MaxAttempts == 0 {
		sub.MaxAttempts = 3
	}
	if sub.MaxAttempts < 1 || sub.MaxAttempts > 20 {
		http.Error(w, "max_attempts must be 1..20", http.StatusBadRequest)
		return
	}
	var key *string
	if sub.IdempotencyKey != "" {
		key = &sub.IdempotencyKey
	}
	// The spec is stored as parsed and re-encoded, not as received, so the
	// stored form is exactly the one spec_sha256 was computed over.
	stored, _ := json.Marshal(sp)
	id := newID()
	_, err = s.DB.Exec(r.Context(), `
		INSERT INTO experiments (id, idempotency_key, spec, spec_sha256, max_attempts)
		VALUES ($1, $2, $3, $4, $5)`, id, key, stored, digest, sub.MaxAttempts)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && key != nil {
		// A resubmission with the same key is the same experiment, but only if
		// it is the same spec. The same key on a different spec is a client bug
		// that would otherwise silently discard the second spec.
		var existing, existingSHA string
		if err := s.DB.QueryRow(r.Context(), `SELECT id, spec_sha256 FROM experiments WHERE idempotency_key = $1`, *key).Scan(&existing, &existingSHA); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if existingSHA != digest {
			http.Error(w, "idempotency_key already used for a different spec", http.StatusConflict)
			return
		}
		s.writeExperiment(w, r.Context(), existing, http.StatusOK)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeExperiment(w, r.Context(), id, http.StatusCreated)
}

func (s *Server) getExperiment(w http.ResponseWriter, r *http.Request) {
	s.writeExperiment(w, r.Context(), r.PathValue("id"), http.StatusOK)
}

func (s *Server) writeExperiment(w http.ResponseWriter, ctx context.Context, id string, code int) {
	e, err := LoadExperiment(ctx, s.DB, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, nil)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, code, e)
}

func LoadExperiment(ctx context.Context, db *pgxpool.Pool, id string) (Experiment, error) {
	var e Experiment
	err := db.QueryRow(ctx, `
		SELECT id, state, status_reason, attempt, max_attempts, spec_sha256, rig_id, fence, submitted_at
		  FROM experiments WHERE id = $1`, id).Scan(
		&e.ID, &e.State, &e.StatusReason, &e.Attempt, &e.MaxAttempts, &e.SpecSHA256, &e.RigID, &e.Fence, &e.SubmittedAt)
	if err != nil {
		return e, err
	}
	rows, err := db.Query(ctx, `
		SELECT attempt, rig_id, fence, scheduler, status, status_reason, leased_at, finished_at
		  FROM attempts WHERE experiment_id = $1 ORDER BY attempt`, id)
	if err != nil {
		return e, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.Attempt, &a.RigID, &a.Fence, &a.Scheduler, &a.Status, &a.StatusReason, &a.LeasedAt, &a.FinishedAt); err != nil {
			return e, err
		}
		e.Attempts = append(e.Attempts, a)
	}
	return e, rows.Err()
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	attempt, err := strconv.Atoi(r.PathValue("attempt"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var c wire.Completion
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch c.Status {
	case artifact.Succeeded, artifact.Failed, artifact.Invalid:
	default:
		http.Error(w, "unknown status", http.StatusBadRequest)
		return
	}
	err = sched.Complete(r.Context(), s.DB, r.PathValue("id"), attempt, c)
	switch {
	case errors.Is(err, sched.ErrUnknownAttempt):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, sched.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		s.completions.WithLabelValues(c.Status).Inc()
		w.WriteHeader(http.StatusNoContent)
	}
}

// heartbeat registers the rig on first contact, refreshes what it advertises,
// and renews the lease for every attempt the agent says it is running. The
// agent, not the scheduler, keeps a lease alive: the scheduler that placed an
// attempt can die without the attempt being lost, and a lease outlives its
// TTL only while the rig itself vouches for the work.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var hb wire.Heartbeat
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&hb); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	if hb.Descriptor.RigID != id {
		http.Error(w, "descriptor rig_id does not match path", http.StatusBadRequest)
		return
	}
	if hb.AgentState != "READY" && hb.AgentState != "QUARANTINED" {
		http.Error(w, "unknown agent_state", http.StatusBadRequest)
		return
	}
	desc, _ := json.Marshal(hb.Descriptor)
	if _, err := s.DB.Exec(r.Context(), `
		INSERT INTO rigs (id, descriptor, agent_state, agent_reason, last_heartbeat)
		VALUES ($1, $2, $3, $4, clock_timestamp())
		ON CONFLICT (id) DO UPDATE
		   SET descriptor = EXCLUDED.descriptor, agent_state = EXCLUDED.agent_state,
		       agent_reason = EXCLUDED.agent_reason, last_heartbeat = clock_timestamp()`,
		id, desc, hb.AgentState, hb.AgentReason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range hb.Active {
		if _, err := lease.Renew(r.Context(), s.DB, id, a.Fence, s.LeaseTTL); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.heartbeats.Inc()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRigs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `
		SELECT id, descriptor, agent_state, agent_reason, last_heartbeat, fence, holder, experiment_id, expires_at
		  FROM rigs ORDER BY id`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type rig struct {
		ID            string          `json:"id"`
		Descriptor    json.RawMessage `json:"descriptor"`
		AgentState    string          `json:"agent_state"`
		AgentReason   string          `json:"agent_reason"`
		LastHeartbeat time.Time       `json:"last_heartbeat"`
		Fence         int64           `json:"fence"`
		Holder        *string         `json:"holder"`
		ExperimentID  *string         `json:"experiment_id"`
		ExpiresAt     *time.Time      `json:"expires_at"`
	}
	out := []rig{}
	for rows.Next() {
		var x rig
		if err := rows.Scan(&x.ID, &x.Descriptor, &x.AgentState, &x.AgentReason, &x.LastHeartbeat, &x.Fence, &x.Holder, &x.ExperimentID, &x.ExpiresAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	digest, err := s.Store.PutBlob(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sha256": digest})
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.BlobPath(r.PathValue("digest"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, p)
}

func attemptFromPath(r *http.Request) (string, int, bool) {
	a, ok := strings.CutPrefix(r.PathValue("attempt"), "attempt-")
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(a)
	if err != nil || strconv.Itoa(n) != a {
		return "", 0, false
	}
	return r.PathValue("run"), n, true
}

func (s *Server) putArtifact(w http.ResponseWriter, r *http.Request) {
	run, attempt, ok := attemptFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := s.Store.PutFile(run, attempt, r.PathValue("file"), r.Header.Get(wire.SHA256Header), r.Body)
	writeStoreErr(w, err)
}

func (s *Server) seal(w http.ResponseWriter, r *http.Request) {
	run, attempt, ok := attemptFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeStoreErr(w, s.Store.Seal(run, attempt, b))
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, artifact.ErrSealed):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, artifact.ErrDigest):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// stateCollector reports queue depth and rig health from the database at
// scrape time, so every replica reports the same numbers and none of them has
// to keep a counter in step with the others.
type stateCollector struct{ db *pgxpool.Pool }

var (
	experimentsDesc = prometheus.NewDesc("benchgrid_experiments", "Experiments by state.", []string{"state"}, nil)
	rigsDesc        = prometheus.NewDesc("benchgrid_rigs", "Rigs by agent state and lease.", []string{"agent_state", "leased"}, nil)
)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- experimentsDesc
	ch <- rigsDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if rows, err := c.db.Query(ctx, `SELECT state, count(*) FROM experiments GROUP BY state`); err == nil {
		for rows.Next() {
			var st string
			var n int64
			if rows.Scan(&st, &n) == nil {
				ch <- prometheus.MustNewConstMetric(experimentsDesc, prometheus.GaugeValue, float64(n), st)
			}
		}
		rows.Close()
	}
	if rows, err := c.db.Query(ctx, `
		SELECT agent_state, (holder IS NOT NULL AND expires_at > clock_timestamp())::text, count(*)
		  FROM rigs GROUP BY 1, 2`); err == nil {
		for rows.Next() {
			var st, leased string
			var n int64
			if rows.Scan(&st, &leased, &n) == nil {
				ch <- prometheus.MustNewConstMetric(rigsDesc, prometheus.GaugeValue, float64(n), st, leased)
			}
		}
		rows.Close()
	}
}
