package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/stats"
)

var attemptName = regexp.MustCompile(`^attempt-([1-9][0-9]*)$`)

// Verify applies every rule a consumer applies, so benchgrid finds a broken run
// before TraceLab does. It is deliberately written against the files on disk
// and not against the structs that produced them.
func Verify(dir string) (Run, []Sample, error) {
	fail := func(f string, a ...any) (Run, []Sample, error) {
		return Run{}, nil, fmt.Errorf("%s: "+f, append([]any{dir}, a...)...)
	}
	mb, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return fail("unsealed: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.SchemaVersion != ManifestSchema {
		return fail("manifest unreadable or wrong schema")
	}
	files := map[string][]byte{}
	for _, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			return fail("listed file %s missing", f.Path)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
			return fail("%s does not match its manifest entry", f.Path)
		}
		files[f.Path] = b
	}
	if files[RunFile] == nil || files[SamplesFile] == nil {
		return fail("manifest does not list run.json and samples.jsonl")
	}

	var run Run
	dec := json.NewDecoder(bytes.NewReader(files[RunFile]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&run); err != nil {
		return fail("run.json: %v", err)
	}
	if run.SchemaVersion != RunSchema {
		return fail("run schema %q", run.SchemaVersion)
	}
	match := attemptName.FindStringSubmatch(filepath.Base(dir))
	if match == nil || match[1] != strconv.Itoa(run.Attempt) || filepath.Base(filepath.Dir(dir)) != run.RunID {
		return fail("run_id %s attempt %d do not match the path", run.RunID, run.Attempt)
	}
	if h, err := spec.Spec.SHA256(run.Spec); err != nil || h != run.SpecSHA256 {
		return fail("spec_sha256 does not match the embedded spec")
	}
	env := run.Environment
	wantConfig := run.Spec.Artifacts.ConfigSHA256
	if env.GitRevision != run.Spec.Revision || env.BinarySHA256 != run.Spec.Artifacts.BinarySHA256 ||
		(env.ConfigSHA256 == nil) != (wantConfig == "") || (env.ConfigSHA256 != nil && *env.ConfigSHA256 != wantConfig) {
		return fail("environment provenance does not match the spec")
	}
	switch run.Status {
	case Succeeded:
		if run.StatusReason != "" {
			return fail("SUCCEEDED with a reason")
		}
	case Failed, Invalid:
		if run.StatusReason == "" {
			return fail("%s without a reason", run.Status)
		}
	default:
		return fail("status %q", run.Status)
	}

	samples, err := ReadSamples(bytes.NewReader(files[SamplesFile]))
	if err != nil {
		return fail("%v", err)
	}
	next := map[string]int{}
	for _, s := range samples {
		m, ok := run.Spec.Metric(s.Metric)
		if !ok || m.Unit != s.Unit {
			return fail("sample metric %s unit %s not declared", s.Metric, s.Unit)
		}
		if s.Iteration != next[s.Metric] {
			return fail("metric %s iteration %d, expected %d", s.Metric, s.Iteration, next[s.Metric])
		}
		next[s.Metric]++
	}

	want := Summarize(run.Spec, samples)
	if len(want) != len(run.Summary) {
		return fail("summary has %d metrics, spec declares %d", len(run.Summary), len(want))
	}
	for name, w := range want {
		g, ok := run.Summary[name]
		if !ok || g.Unit != w.Unit || g.N != w.N {
			return fail("summary for %s missing or wrong unit or n", name)
		}
		pairs := [][2]*float64{{g.Mean, w.Mean}, {g.Median, w.Median}, {g.P90, w.P90}, {g.P95, w.P95}, {g.P99, w.P99}, {g.Stddev, w.Stddev}, {g.MAD, w.MAD}, {g.CV, w.CV}}
		for _, p := range pairs {
			if (p[0] == nil) != (p[1] == nil) || (p[0] != nil && !stats.Agree(*p[0], *p[1])) {
				return fail("summary for %s disagrees with its samples", name)
			}
		}
	}
	return run, samples, nil
}
