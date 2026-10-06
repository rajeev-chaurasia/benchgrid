// evidence produces every number the README publishes, with the raw data
// behind each one. Run it, commit its output directory, and the validator in
// CI recomputes the numbers from the raw files and refuses an edited one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

type harness struct {
	out     string
	bin     string
	pgBase  string
	scratch string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stress" {
		stress(os.Args[2:])
		return
	}
	out := flag.String("out", "evidence/results", "parent directory for this run's results")
	runs := flag.String("runs", "lease,fence,chaos,noise", "which runs to perform")
	bin := flag.String("bin", "bin", "directory holding built binaries")
	pg := flag.String("pg", "postgres:///postgres?sslmode=disable", "Postgres maintenance URL; each run creates its own database")
	scratch := flag.String("scratch", "", "where agents keep state; defaults to a temp dir")
	flag.Parse()

	stamp := time.Now().UTC().Format("20060102T150405Z")
	h := &harness{out: filepath.Join(*out, stamp), bin: *bin, pgBase: *pg, scratch: *scratch}
	if h.scratch == "" {
		dir, err := os.MkdirTemp("", "benchgrid-evidence-")
		if err != nil {
			log.Fatal(err)
		}
		h.scratch = dir
	}
	if err := os.MkdirAll(h.out, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := evidence.WriteJSON(filepath.Join(h.out, "env.json"), h.environment()); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range strings.Split(*runs, ",") {
		start := time.Now()
		log.Printf("run %s", r)
		var err error
		switch r {
		case "lease":
			err = h.leaseRace(ctx)
		case "fence":
			err = h.fence(ctx)
		case "chaos":
			err = h.chaos(ctx)
		case "noise":
			err = h.noise(ctx)
		default:
			err = fmt.Errorf("unknown run %q", r)
		}
		if err != nil {
			log.Fatalf("run %s: %v", r, err)
		}
		log.Printf("run %s done in %s", r, time.Since(start).Round(time.Second))
	}
	if err := evidence.WriteManifest(h.out); err != nil {
		log.Fatal(err)
	}
	fmt.Println(h.out)
}

// Environment is what a reader needs to judge whether the numbers transfer to
// their machine. Every field is read from the system, not typed in.
type Environment struct {
	GitCommit   string `json:"git_commit"`
	GitDirty    bool   `json:"git_dirty"`
	GoVersion   string `json:"go_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	CPU         string `json:"cpu"`
	CPUCores    int    `json:"cpu_cores"`
	MemBytes    string `json:"mem_bytes"`
	Kernel      string `json:"kernel"`
	Postgres    string `json:"postgres"`
	StartedUTC  string `json:"started_utc"`
	Description string `json:"description"`
}

func (h *harness) environment() Environment {
	run := func(name string, args ...string) string {
		b, _ := exec.Command(name, args...).Output()
		return strings.TrimSpace(string(b))
	}
	e := Environment{
		GitCommit:  run("git", "rev-parse", "HEAD"),
		GitDirty:   run("git", "status", "--porcelain") != "",
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUCores:   runtime.NumCPU(),
		Kernel:     run("uname", "-r"),
		Postgres:   run("psql", h.pgBase, "-tAc", "SHOW server_version"),
		StartedUTC: time.Now().UTC().Format(time.RFC3339),
		Description: "Every rig is a process on this one host. No physical rig and no GPU took " +
			"part. Rigs are distinguished by agent, not by hardware.",
	}
	if runtime.GOOS == "darwin" {
		e.CPU = run("sysctl", "-n", "machdep.cpu.brand_string")
		e.MemBytes = run("sysctl", "-n", "hw.memsize")
	} else {
		e.CPU = run("sh", "-c", "grep -m1 'model name' /proc/cpuinfo | cut -d: -f2")
		e.MemBytes = run("sh", "-c", "awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo")
	}
	return e
}
