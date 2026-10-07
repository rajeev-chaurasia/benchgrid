package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

// proc is one long-running process the harness owns and may kill, freeze,
// and restart with the same arguments.
type proc struct {
	name string
	path string
	args []string
	log  string

	// container names a Docker container this proc runs. Signals sent to the
	// docker client do not reach the container, so kill removes it instead.
	container string

	mu  sync.Mutex
	cmd *exec.Cmd
}

func (p *proc) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.path, p.args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	p.cmd = cmd
	go func() { cmd.Wait(); f.Close() }()
	return nil
}

func (p *proc) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *proc) signal(sig syscall.Signal) {
	if pid := p.pid(); pid > 0 {
		syscall.Kill(pid, sig)
	}
}

func (p *proc) kill() {
	if p.container != "" {
		exec.Command("docker", "rm", "-f", p.container).Run()
		return
	}
	p.signal(syscall.SIGCONT)
	p.signal(syscall.SIGKILL)
	time.Sleep(50 * time.Millisecond)
}

func (p *proc) stopped() bool {
	pid := p.pid()
	if pid == 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T")
}

type cluster struct {
	h          *harness
	name       string
	db         *pgxpool.Pool
	dir        string
	store      string
	servers    []*proc
	agents     []*proc
	serverURLs []string
	blobs      map[string]string
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type clusterConfig struct {
	replicas   int
	serverArgs []string
	rigs       int
	agentArgs  func(i int) []string
	// linuxImage, when set, runs every agent as a Linux container from this
	// image instead of as a process on the host.
	linuxImage string
}

func (h *harness) newCluster(ctx context.Context, name string, cfg clusterConfig) (*cluster, error) {
	db, dbURL, err := h.freshDB(ctx, "benchgrid_evidence_"+name)
	if err != nil {
		return nil, err
	}
	c := &cluster{h: h, name: name, db: db, dir: filepath.Join(h.scratch, name)}
	os.RemoveAll(c.dir)
	c.store = filepath.Join(c.dir, "store")
	if err := os.MkdirAll(c.store, 0o755); err != nil {
		return nil, err
	}
	for i := 0; i < cfg.replicas; i++ {
		port := freePort()
		url := fmt.Sprintf("http://127.0.0.1:%d", port)
		args := append([]string{"-listen", fmt.Sprintf("127.0.0.1:%d", port), "-db", dbURL,
			"-artifacts", c.store, "-id", fmt.Sprintf("%s-s%d", name, i)}, cfg.serverArgs...)
		p := &proc{name: fmt.Sprintf("server-%d", i), path: filepath.Join(h.bin, "benchgrid"), args: args,
			log: filepath.Join(c.dir, fmt.Sprintf("server-%d.log", i))}
		if err := p.start(); err != nil {
			return nil, err
		}
		c.servers = append(c.servers, p)
		c.serverURLs = append(c.serverURLs, url)
	}
	for _, u := range c.serverURLs {
		if err := waitHTTP(u+"/healthz", 20*time.Second); err != nil {
			return nil, err
		}
	}
	for i := 0; i < cfg.rigs; i++ {
		port := freePort()
		id := fmt.Sprintf("rig-%02d", i)
		var extra []string
		if cfg.agentArgs != nil {
			extra = cfg.agentArgs(i)
		}
		var p *proc
		if cfg.linuxImage == "" {
			args := append([]string{"-id", id, "-state-dir", filepath.Join(c.dir, id),
				"-control", strings.Join(c.serverURLs, ","),
				"-listen", fmt.Sprintf("127.0.0.1:%d", port), "-endpoint", fmt.Sprintf("http://127.0.0.1:%d", port),
				"-interval-log", filepath.Join(c.dir, id+".intervals.jsonl"), "-heartbeat", "500ms"}, extra...)
			p = &proc{name: id, path: filepath.Join(h.bin, "rigagent"), args: args, log: filepath.Join(c.dir, id+".log")}
		} else {
			name := "benchgrid-" + name + "-" + id
			exec.Command("docker", "rm", "-f", name).Run()
			var control []string
			for _, u := range c.serverURLs {
				control = append(control, strings.Replace(u, "127.0.0.1", "host.docker.internal", 1))
			}
			args := append([]string{"run", "--rm", "--name", name,
				"--add-host", "host.docker.internal:host-gateway",
				"-v", c.dir + ":/work", "-p", fmt.Sprintf("127.0.0.1:%d:9090", port),
				cfg.linuxImage,
				"-id", id, "-state-dir", "/work/" + id, "-control", strings.Join(control, ","),
				"-listen", ":9090", "-endpoint", fmt.Sprintf("http://127.0.0.1:%d", port),
				"-interval-log", "/work/" + id + ".intervals.jsonl", "-heartbeat", "500ms"}, extra...)
			p = &proc{name: id, path: "docker", args: args, log: filepath.Join(c.dir, id+".log"), container: name}
		}
		if err := p.start(); err != nil {
			return nil, err
		}
		c.agents = append(c.agents, p)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		db.QueryRow(ctx, `SELECT count(*) FROM rigs WHERE agent_state = 'READY'`).Scan(&n)
		if n == cfg.rigs {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("only %d of %d rigs registered", n, cfg.rigs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.blobs = map[string]string{}
	for osName, path := range map[string]string{runtime.GOOS: filepath.Join(h.bin, "benchload"), "linux": filepath.Join(h.scratch, "linux", "benchload")} {
		b, err := os.ReadFile(path)
		if err != nil {
			if osName == "linux" && cfg.linuxImage == "" {
				continue
			}
			return nil, err
		}
		var out struct {
			SHA256 string `json:"sha256"`
		}
		if err := postJSON(c.serverURLs[0]+"/v1/blobs", b, &out); err != nil {
			return nil, err
		}
		c.blobs[osName] = out.SHA256
	}
	return c, nil
}

func (c *cluster) close() {
	for _, p := range append(c.agents, c.servers...) {
		p.kill()
	}
	c.db.Close()
}

func (c *cluster) submit(s spec.Spec, key string) (string, error) {
	return c.submitWith(s, key, 0)
}

// submitOnce allows a single attempt. The noise run measures what one
// attempt produces under each condition; a retry after an INVALID verdict
// would be a second sample of the noise, counted as if it were the first.
func (c *cluster) submitOnce(s spec.Spec, key string) (string, error) {
	return c.submitWith(s, key, 1)
}

func (c *cluster) submitWith(s spec.Spec, key string, maxAttempts int) (string, error) {
	target := s.Requirements.OS
	if target == "" {
		target = runtime.GOOS
	}
	s.Artifacts.BinarySHA256 = c.blobs[target]
	body, _ := json.Marshal(map[string]any{"spec": s, "idempotency_key": key, "max_attempts": maxAttempts})
	var e struct {
		ID string `json:"id"`
	}
	var err error
	for _, u := range c.serverURLs {
		if err = postJSON(u+"/v1/experiments", body, &e); err == nil {
			return e.ID, nil
		}
	}
	return "", err
}

// drain waits until every experiment is in a terminal state.
func (c *cluster) drain(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var open int
		if err := c.db.QueryRow(ctx, `SELECT count(*) FROM experiments WHERE state IN ('QUEUED', 'RUNNING')`).Scan(&open); err != nil {
			return err
		}
		if open == 0 {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("experiments still open after %s", timeout)
}

func waitHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s not healthy", url)
}

func postJSON(url string, body []byte, into any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, into)
}

func benchSpec(rev string, args ...string) spec.Spec {
	return spec.Spec{
		Benchmark: "cpu_hash", Revision: rev,
		Command: append([]string{spec.BinaryPlaceholder}, args...),
		Warmups: 1, Repetitions: 2, TimeoutSeconds: 60,
		Metrics: []spec.Metric{
			{Name: "iteration_latency", Unit: "ns", Direction: "lower_is_better"},
			{Name: "max_rss", Unit: "bytes", Direction: "lower_is_better"},
			{Name: "throughput", Unit: "ops_per_s", Direction: "higher_is_better"},
		},
	}
}
