// rigagent owns one rig. It runs on the rig's host as a long-lived service,
// not as a pod: a rig is scarce hardware with an owner, not capacity.
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

	"github.com/rajeev-chaurasia/benchgrid/internal/agent"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/telemetry"
)

func main() {
	id := flag.String("id", "", "rig id")
	stateDir := flag.String("state-dir", "", "durable state: fence mark, spool, local runs")
	control := flag.String("control", "http://127.0.0.1:8080", "comma separated control plane URLs")
	listen := flag.String("listen", ":9090", "address the control plane dispatches to")
	endpoint := flag.String("endpoint", "", "URL the control plane should use to reach this agent")
	profilePath := flag.String("profile", "", "emulation profile; any profile marks the rig emulated")
	intervalLog := flag.String("interval-log", "", "append every process and session interval here")
	heartbeat := flag.Duration("heartbeat", time.Second, "heartbeat interval")
	timeout := flag.Duration("request-timeout", 2*time.Second, "per request timeout to the control plane; keep well inside the lease TTL")
	unfenced := flag.Bool("unfenced-negative-control", false, "evidence harness only: accept every dispatch regardless of fence")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("rig", *id)
	if *id == "" || *stateDir == "" || *endpoint == "" {
		log.Error("-id, -state-dir and -endpoint are required")
		os.Exit(2)
	}
	prober := &probe.Prober{}
	if *profilePath != "" {
		p, err := probe.LoadProfile(*profilePath)
		if err != nil {
			log.Error("profile", "err", err)
			os.Exit(2)
		}
		prober.Profile = p
	}
	if *unfenced {
		log.Warn("running WITHOUT fencing; this agent exists to demonstrate the failure")
	}
	a, err := agent.New(agent.Config{
		RigID: *id, StateDir: *stateDir, ControlURLs: strings.Split(*control, ","),
		Endpoint: *endpoint, Fenced: !*unfenced, Prober: prober,
		HeartbeatEvery: *heartbeat, RequestTimeout: *timeout, IntervalLog: *intervalLog, Logger: log,
	})
	if err != nil {
		log.Error("agent", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.Setup(ctx, "rigagent")
	if err != nil {
		log.Error("tracing", "err", err)
		os.Exit(1)
	}
	defer shutdownTracing(context.Background())

	srv := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	go a.Run(ctx)
	log.Info("agent ready", "listen", *listen, "fenced", !*unfenced)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
