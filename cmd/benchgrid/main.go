// benchgrid is the control plane: the HTTP API and, unless disabled, a
// scheduler loop. Run as many as you like against one database.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/sched"
	"github.com/rajeev-chaurasia/benchgrid/internal/server"
	"github.com/rajeev-chaurasia/benchgrid/internal/store"
	"github.com/rajeev-chaurasia/benchgrid/internal/telemetry"
)

func main() {
	listen := flag.String("listen", ":8080", "address to serve the API on")
	db := flag.String("db", envOr("BENCHGRID_DATABASE_URL", "postgres:///benchgrid?sslmode=disable"), "Postgres URL")
	artifacts := flag.String("artifacts", "var/store", "artifact store: a directory, or gs://bucket/prefix")
	id := flag.String("id", hostname(), "scheduler instance id, recorded on every attempt it places")
	ttl := flag.Duration("lease-ttl", 5*time.Second, "lease TTL; agents renew through heartbeats")
	tick := flag.Duration("tick", 200*time.Millisecond, "scheduling interval")
	noSched := flag.Bool("no-scheduler", false, "serve the API only")
	freeze := flag.Float64("fault-freeze-before-dispatch", 0, "evidence harness only: probability of SIGSTOP between lease and dispatch")
	artifactFaults := flag.Float64("fault-artifact-error-rate", 0, "evidence harness only: fraction of artifact writes answered 503")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("instance", *id)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.Setup(ctx, "benchgrid")
	if err != nil {
		log.Error("tracing", "err", err)
		os.Exit(1)
	}
	defer shutdownTracing(context.Background())

	pool, err := store.Open(ctx, *db, 20)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	var store artifact.Store = &artifact.FSStore{Root: *artifacts}
	if strings.HasPrefix(*artifacts, "gs://") {
		gcs, err := artifact.NewGCSStore(ctx, *artifacts)
		if err != nil {
			log.Error("artifact store", "err", err)
			os.Exit(1)
		}
		store = gcs
	}
	srv := &server.Server{DB: pool, Store: store, LeaseTTL: *ttl, Log: log, Registry: reg, FaultArtifactErrorRate: *artifactFaults}
	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}

	if !*noSched {
		s := sched.New(sched.Config{ID: *id, LeaseTTL: *ttl, Tick: *tick, FreezeBeforeDispatch: *freeze, Logger: log}, pool, sched.NewMetrics(reg))
		go s.Run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()
	log.Info("serving", "listen", *listen, "scheduler", !*noSched)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
