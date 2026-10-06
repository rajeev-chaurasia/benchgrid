package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

// pending is a finished attempt the control plane has not yet acknowledged.
// It lives on disk so that a control plane outage, or an agent restart during
// one, loses nothing: the run directory and this record stay until the
// completion is accepted.
type pending struct {
	ExperimentID string `json:"experiment_id"`
	Attempt      int    `json:"attempt"`
	Fence        int64  `json:"fence"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	Dir          string `json:"dir"`
	Uploaded     bool   `json:"uploaded"`
}

func (a *Agent) collect(s *session, out outcome) error {
	sp := s.d.Spec
	h, err := sp.SHA256()
	if err != nil {
		return err
	}
	samples := out.samples
	if samples == nil {
		samples = []artifact.Sample{}
	}
	run := artifact.Run{
		SchemaVersion: artifact.RunSchema,
		RunID:         s.d.ExperimentID,
		Attempt:       s.d.Attempt,
		Fence:         s.fence,
		Status:        out.status,
		StatusReason:  out.reason,
		SpecSHA256:    h,
		Spec:          sp,
		Rig:           artifact.RigFrom(a.Descriptor()),
		Environment: artifact.Environment{
			GitRevision:     sp.Revision,
			BinarySHA256:    sp.Artifacts.BinarySHA256,
			ConfigSHA256:    sp.Artifacts.ConfigSHA256,
			Governor:        out.governor,
			PreflightBefore: out.before,
			PreflightAfter:  out.after,
		},
		Timing: artifact.Timing{
			LeaseAcquired: s.d.LeaseAcquired,
			Started:       out.started.UTC().Format(Timestamp),
			Finished:      time.Now().UTC().Format(Timestamp),
		},
		Summary: artifact.Summarize(sp, samples),
	}
	rj, sj, err := artifact.Encode(run, samples)
	if err != nil {
		return err
	}
	dir := artifact.AttemptDir(filepath.Join(a.cfg.StateDir, "runs"), s.d.ExperimentID, s.d.Attempt)
	if err := artifact.WriteDir(dir, map[string][]byte{artifact.RunFile: rj, artifact.SamplesFile: sj}); err != nil {
		return err
	}
	p := pending{ExperimentID: s.d.ExperimentID, Attempt: s.d.Attempt, Fence: s.fence,
		Status: out.status, StatusReason: out.reason, Dir: dir}
	if err := a.writePending(p); err != nil {
		return err
	}
	a.flush(context.Background(), p)
	return nil
}

func (a *Agent) pendingPath(p pending) string {
	return filepath.Join(a.cfg.StateDir, "spool", p.ExperimentID+"-"+strconv.Itoa(p.Attempt)+".json")
}

func (a *Agent) writePending(p pending) error {
	b, _ := json.Marshal(p)
	tmp := a.pendingPath(p) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.pendingPath(p))
}

// flush uploads and then reports one finished attempt. Each step is idempotent
// on the server, so a step that succeeded but whose reply was lost is simply
// repeated.
func (a *Agent) flush(ctx context.Context, p pending) bool {
	if a.cfg.ControlURL == "" {
		return false
	}
	if !p.Uploaded {
		if err := a.upload(ctx, p); err != nil {
			a.log.Warn("upload deferred", "experiment", p.ExperimentID, "attempt", p.Attempt, "err", err)
			return false
		}
		p.Uploaded = true
		a.writePending(p)
	}
	c := wire.Completion{RigID: a.cfg.RigID, Fence: p.Fence, Status: p.Status, StatusReason: p.StatusReason}
	url := fmt.Sprintf("%s/v1/experiments/%s/attempts/%d/complete", a.cfg.ControlURL, p.ExperimentID, p.Attempt)
	if err := a.post(ctx, url, c, nil); err != nil {
		a.log.Warn("completion deferred", "experiment", p.ExperimentID, "err", err)
		return false
	}
	os.Remove(a.pendingPath(p))
	return true
}

func (a *Agent) upload(ctx context.Context, p pending) error {
	base := fmt.Sprintf("%s/v1/artifacts/runs/%s/attempt-%d", a.cfg.ControlURL, p.ExperimentID, p.Attempt)
	for _, name := range []string{artifact.RunFile, artifact.SamplesFile} {
		b, err := os.ReadFile(filepath.Join(p.Dir, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+"/"+name, bytes.NewReader(b))
		req.Header.Set(wire.SHA256Header, hex.EncodeToString(sum[:]))
		if err := a.do(req, nil); err != nil {
			return err
		}
	}
	manifest, err := os.ReadFile(filepath.Join(p.Dir, artifact.ManifestFile))
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/seal", bytes.NewReader(manifest))
	return a.do(req, nil)
}

func (a *Agent) spoolLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		files, _ := filepath.Glob(filepath.Join(a.cfg.StateDir, "spool", "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var p pending
			if json.Unmarshal(b, &p) == nil {
				a.flush(ctx, p)
			}
		}
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.HeartbeatEvery)
	defer t.Stop()
	for {
		a.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) heartbeat(ctx context.Context) {
	if a.cfg.ControlURL == "" {
		return
	}
	hb := a.Snapshot()
	hb.Readings = a.cfg.Prober.Read(ctx)
	if err := a.post(ctx, a.cfg.ControlURL+"/v1/rigs/"+a.cfg.RigID+"/heartbeat", hb, nil); err != nil {
		a.log.Debug("heartbeat failed", "err", err)
	}
}

func (a *Agent) post(ctx context.Context, url string, body, into any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return a.do(req, into)
}

func (a *Agent) do(req *http.Request, into any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if into != nil {
		return json.NewDecoder(resp.Body).Decode(into)
	}
	return nil
}
