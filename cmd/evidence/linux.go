package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

const linuxRigs = 6

// linux repeats the fence run with every agent in a Linux container, so the
// claim is shown on the platform the agent is meant for and the Linux code
// paths, /proc readings, ru_maxrss in kilobytes, and process groups under a
// Linux kernel, run under the same evidence as everything else. The
// containers run in Docker's Linux VM on the same laptop, which is stated
// rather than implied away.
func (h *harness) linux(ctx context.Context) error {
	image, err := h.linuxImage()
	if err != nil {
		return err
	}
	dir := filepath.Join(h.out, "linux")
	var summaries []evidence.FenceSummary
	for _, mode := range []string{"fenced", "unfenced"} {
		s, err := h.fenceMode(ctx, dir, mode, image)
		if err != nil {
			return fmt.Errorf("linux %s: %w", mode, err)
		}
		summaries = append(summaries, s)
	}
	return evidence.WriteJSON(filepath.Join(dir, "summary.json"), summaries)
}

// linuxImage builds an image from scratch holding nothing but the agent and
// the benchmark, statically linked, so nothing is pulled from a registry and
// nothing in the image is unaccounted for.
func (h *harness) linuxImage() (string, error) {
	dir := filepath.Join(h.scratch, "linux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"rigagent", "benchload"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build %s: %s", name, out)
		}
	}
	df := "FROM scratch\nCOPY rigagent benchload /\nENTRYPOINT [\"/rigagent\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		return "", err
	}
	out, err := exec.Command("docker", "build", "-q", "-t", "benchgrid-rig:evidence", dir).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker build: %s", out)
	}
	return strings.TrimSpace(string(out)), nil
}
