// Package evidence holds the arithmetic behind every published number. The
// harness that produces the raw data and the validator that rechecks it both
// call these functions, so a number in a summary and the number recomputed
// from its raw file cannot disagree because of two implementations.
package evidence

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Span is one claim of ownership over a resource: a lease holder's belief
// that it holds a rig, or a process an agent ran under a fence.
type Span struct {
	Resource string `json:"resource"`
	Owner    string `json:"owner"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
}

// OverlapPairs counts pairs of spans on one resource, with different owners,
// that intersect. Spans are half open, so one ending at t and another starting
// at t do not overlap. Two spans with the same owner never count: one holder
// running several processes in sequence is not a double booking.
func OverlapPairs(spans []Span) int {
	by := map[string][]Span{}
	for _, s := range spans {
		by[s.Resource] = append(by[s.Resource], s)
	}
	total := 0
	for _, list := range by {
		sort.Slice(list, func(i, j int) bool { return list[i].Start < list[j].Start })
		var open []Span
		for _, s := range list {
			kept := open[:0]
			for _, o := range open {
				if o.End > s.Start {
					kept = append(kept, o)
				}
			}
			open = kept
			for _, o := range open {
				if o.Owner != s.Owner {
					total++
				}
			}
			open = append(open, s)
		}
	}
	return total
}

// PeakConcurrency is the largest number of spans in progress at one instant,
// regardless of resource. It is published beside every race so a reader can
// see that the race actually raced.
func PeakConcurrency(spans []Span) int {
	type edge struct {
		t int64
		d int
	}
	edges := make([]edge, 0, 2*len(spans))
	for _, s := range spans {
		edges = append(edges, edge{s.Start, 1}, edge{s.End, -1})
	}
	// Ends sort before starts at the same instant, matching the half open rule.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].t != edges[j].t {
			return edges[i].t < edges[j].t
		}
		return edges[i].d < edges[j].d
	})
	peak, cur := 0, 0
	for _, e := range edges {
		cur += e.d
		if cur > peak {
			peak = cur
		}
	}
	return peak
}

func WriteJSONLGz[T any](path string, rows []T) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return gz.Close()
}

func ReadJSONLGz[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return readJSONL[T](gz)
}

func ReadJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readJSONL[T](f)
}

func readJSONL[T any](r io.Reader) ([]T, error) {
	var out []T
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

const ManifestName = "SHA256SUMS"

// WriteManifest records a digest for every file under dir, in sha256sum's own
// format so `shasum -a 256 -c` can check it without this repository.
func WriteManifest(dir string) error {
	lines, err := digests(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), []byte(strings.Join(lines, "")), 0o644)
}

// VerifyManifest fails on a changed file, a missing file, and a file the
// manifest does not list, so a number cannot be edited and a result cannot be
// quietly added.
func VerifyManifest(dir string) error {
	want, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return err
	}
	got, err := digests(dir)
	if err != nil {
		return err
	}
	if string(want) != strings.Join(got, "") {
		return fmt.Errorf("%s does not match the files in %s", ManifestName, dir)
	}
	return nil
}

func digests(dir string) ([]string, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == ManifestName {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+filepath.ToSlash(rel)+"\n")
		return nil
	})
	sort.Slice(lines, func(i, j int) bool { return lines[i][66:] < lines[j][66:] })
	return lines, err
}
