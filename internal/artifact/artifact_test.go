package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

func fixture(t *testing.T) (Run, []Sample) {
	t.Helper()
	s := spec.Spec{
		Benchmark: "cpu_hash", Revision: "8f3c2aa0b6d1e4f7a9c3b5d7e9f1a3c5b7d9e1f3", Command: []string{"{binary}"},
		Warmups: 2, Repetitions: 4, TimeoutSeconds: 60,
		Metrics: []spec.Metric{
			{Name: "iteration_latency", Unit: "ns", Direction: "lower_is_better"},
			{Name: "max_rss", Unit: "bytes", Direction: "lower_is_better"},
		},
		Artifacts: spec.Artifacts{BinarySHA256: strings.Repeat("a", 64)},
	}
	h, _ := s.SHA256()
	var samples []Sample
	for i := 0; i < 6; i++ {
		samples = append(samples,
			Sample{Metric: "iteration_latency", Iteration: i, Warmup: i < 2, Value: float64(1000 + i*7%5), Unit: "ns", TOffsetNS: int64(i * 1000)},
			Sample{Metric: "max_rss", Iteration: i, Warmup: i < 2, Value: 4096, Unit: "bytes", TOffsetNS: int64(i * 1000)},
		)
	}
	run := Run{
		SchemaVersion: RunSchema, RunID: "exp_test", Attempt: 2, Fence: 9,
		Status: Succeeded, SpecSHA256: h, Spec: s, Summary: Summarize(s, samples),
	}
	return run, samples
}

func write(t *testing.T, root string, run Run, samples []Sample) string {
	t.Helper()
	rj, sj, err := Encode(run, samples)
	if err != nil {
		t.Fatal(err)
	}
	dir := AttemptDir(root, run.RunID, run.Attempt)
	if err := WriteDir(dir, map[string][]byte{RunFile: rj, SamplesFile: sj}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRoundTrip(t *testing.T) {
	run, samples := fixture(t)
	dir := write(t, t.TempDir(), run, samples)
	got, gs, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary["iteration_latency"].N != 4 || len(gs) != 12 {
		t.Errorf("warmups leaked into the summary: %+v", got.Summary["iteration_latency"])
	}
}

// Each case breaks one rule and must be the only thing that fails, so every
// rejection path in Verify is shown to be reachable.
func TestVerifyRejects(t *testing.T) {
	cases := map[string]func(*Run, *[]Sample){
		"summary disagrees": func(r *Run, _ *[]Sample) {
			s := r.Summary["iteration_latency"]
			v := *s.Median + 1
			s.Median = &v
			r.Summary["iteration_latency"] = s
		},
		"spec hash stale":       func(r *Run, _ *[]Sample) { r.Spec.Repetitions = 5 },
		"undeclared metric":     func(_ *Run, s *[]Sample) { (*s)[0].Metric = "power" },
		"wrong unit":            func(_ *Run, s *[]Sample) { (*s)[1].Unit = "count" },
		"iteration gap":         func(_ *Run, s *[]Sample) { (*s)[2].Iteration = 5 },
		"attempt not path":      func(r *Run, _ *[]Sample) { r.Attempt = 3 },
		"succeeded with reason": func(r *Run, _ *[]Sample) { r.StatusReason = "timeout" },
		"failed without reason": func(r *Run, _ *[]Sample) { r.Status = Failed },
	}
	for label, mutate := range cases {
		run, samples := fixture(t)
		mutate(&run, &samples)
		root := t.TempDir()
		rj, sj, _ := Encode(run, samples)
		dir := AttemptDir(root, "exp_test", 2)
		WriteDir(dir, map[string][]byte{RunFile: rj, SamplesFile: sj})
		if _, _, err := Verify(dir); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestVerifyRejectsTamperAndUnsealed(t *testing.T) {
	run, samples := fixture(t)
	dir := write(t, t.TempDir(), run, samples)
	b, _ := os.ReadFile(filepath.Join(dir, SamplesFile))
	os.WriteFile(filepath.Join(dir, SamplesFile), bytes.Replace(b, []byte("1000"), []byte("1001"), 1), 0o644)
	if _, _, err := Verify(dir); err == nil {
		t.Error("accepted a tampered samples file")
	}
	os.Remove(filepath.Join(dir, ManifestFile))
	if _, _, err := Verify(dir); err == nil {
		t.Error("accepted an unsealed directory")
	}
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestStoreUploadIsIdempotentAndSealIsFinal(t *testing.T) {
	run, samples := fixture(t)
	rj, sj, _ := Encode(run, samples)
	st := &FSStore{Root: t.TempDir()}

	if err := st.PutFile("exp_test", 2, RunFile, digest(rj), bytes.NewReader(rj)); err != nil {
		t.Fatal(err)
	}
	manifest, _ := BuildManifest(map[string][]byte{RunFile: rj, SamplesFile: sj})
	if err := st.Seal("exp_test", 2, manifest); err == nil {
		t.Fatal("sealed with a listed file missing")
	}
	if err := st.PutFile("exp_test", 2, SamplesFile, digest(rj), bytes.NewReader(sj)); !errors.Is(err, ErrDigest) {
		t.Fatalf("accepted body that does not match its digest: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := st.PutFile("exp_test", 2, SamplesFile, digest(sj), bytes.NewReader(sj)); err != nil {
			t.Fatal(err)
		}
		if err := st.Seal("exp_test", 2, manifest); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	if _, _, err := Verify(AttemptDir(st.Root, "exp_test", 2)); err != nil {
		t.Fatal(err)
	}
	other := append([]byte(nil), sj...)
	other[0] = ' '
	if err := st.PutFile("exp_test", 2, SamplesFile, digest(other), bytes.NewReader(other)); !errors.Is(err, ErrSealed) {
		t.Errorf("overwrote a sealed file: %v", err)
	}
	if err := st.PutFile("../x", 1, RunFile, digest(rj), bytes.NewReader(rj)); err == nil {
		t.Error("accepted a path traversal run id")
	}
}
