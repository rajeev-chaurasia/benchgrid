package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// IterationResult is one execution of the benchmark command.
type IterationResult struct {
	PID       int
	StartNS   int64
	EndNS     int64
	WallNS    int64
	UserNS    int64
	SysNS     int64
	MaxRSS    int64
	Reported  map[string]float64
	ExitCode  int
	Signal    string
	Stderr    string
	Cancelled bool
}

const metricPrefix = "BENCHGRID_METRIC "

// grace is how long a benchmark gets between SIGTERM and SIGKILL. Long enough
// to flush a profiler, short enough that a timeout does not hold the rig.
var grace = 2 * time.Second

// runIteration starts the command in its own process group, so that kill
// reaches every child it forked, not only the direct child. A benchmark that
// spawns workers and is killed by pid alone leaves them running on the rig,
// which is exactly the contamination the next holder's preflight exists to
// find.
type iteration struct {
	argv  []string
	dir   string
	env   []string
	clock func() int64
	// beforeStart runs before the process exists and onStart as soon as it
	// does, which is where the agent writes the markers that let a successor
	// find the process if this agent dies.
	beforeStart func()
	onStart     func(pid int, startNS int64)
}

func runIteration(ctx context.Context, it iteration) (IterationResult, error) {
	argv, clock, beforeStart, onStart := it.argv, it.clock, it.beforeStart, it.onStart
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = it.dir, it.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 16 << 10}

	if beforeStart != nil {
		beforeStart()
	}
	r := IterationResult{StartNS: clock()}
	wallStart := time.Now()
	if err := cmd.Start(); err != nil {
		return r, err
	}
	r.PID = cmd.Process.Pid
	if onStart != nil {
		onStart(r.PID, r.StartNS)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		r.Cancelled = true
		waitErr = terminate(cmd.Process.Pid, done)
	}
	r.WallNS = time.Since(wallStart).Nanoseconds()
	r.EndNS = clock()
	// Children that outlived the leader are still in the group. Reap the group
	// unconditionally so an iteration's end really is the end of its work.
	killGroup(r.PID, syscall.SIGKILL)

	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && ru != nil {
		r.UserNS = timevalNS(ru.Utime)
		r.SysNS = timevalNS(ru.Stime)
		r.MaxRSS = maxRSSBytes(ru.Maxrss)
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			r.Signal = ws.Signal().String()
		}
		r.ExitCode = ws.ExitStatus()
	}
	r.Stderr = stderr.String()

	var reportErr error
	r.Reported, reportErr = parseReported(stdout.Bytes())
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return r, waitErr
	}
	return r, reportErr
}

func terminate(pid int, done <-chan error) error {
	killGroup(pid, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		killGroup(pid, syscall.SIGKILL)
		return <-done
	}
}

func killGroup(pgid int, sig syscall.Signal) {
	if pgid > 0 {
		syscall.Kill(-pgid, sig)
	}
}

// groupAlive reports whether any process remains in the group, which is how
// cleanup proves it actually cleaned up.
func groupAlive(pgid int) bool {
	return pgid > 0 && syscall.Kill(-pgid, 0) == nil
}

func timevalNS(tv syscall.Timeval) int64 {
	return int64(tv.Sec)*1e9 + int64(tv.Usec)*1e3
}

// ru_maxrss is bytes on Darwin and kilobytes on Linux. Getting this wrong
// produces a memory number off by 1024 that still looks plausible.
func maxRSSBytes(v int64) int64 {
	if runtime.GOOS == "darwin" {
		return v
	}
	return v * 1024
}

func parseReported(out []byte) (map[string]float64, error) {
	m := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, metricPrefix) {
			continue
		}
		f := strings.Fields(line[len(metricPrefix):])
		if len(f) != 2 {
			return nil, fmt.Errorf("malformed metric line %q", line)
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return nil, fmt.Errorf("malformed metric value %q", line)
		}
		if _, dup := m[f[0]]; dup {
			return nil, fmt.Errorf("metric %s reported twice in one iteration", f[0])
		}
		m[f[0]] = v
	}
	return m, nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
