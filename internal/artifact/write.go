package artifact

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

func AttemptDir(root, runID string, attempt int) string {
	return filepath.Join(root, "runs", runID, "attempt-"+strconv.Itoa(attempt))
}

// Encode renders the two content files exactly as they will be stored, so the
// bytes hashed into the manifest are the bytes uploaded.
func Encode(run Run, samples []Sample) (runJSON, samplesJSONL []byte, err error) {
	runJSON, err = json.MarshalIndent(run, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	runJSON = append(runJSON, '\n')
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, s := range samples {
		if err := enc.Encode(s); err != nil {
			return nil, nil, err
		}
	}
	return runJSON, buf.Bytes(), nil
}

func BuildManifest(files map[string][]byte) ([]byte, error) {
	m := Manifest{SchemaVersion: ManifestSchema}
	for path, b := range files {
		sum := sha256.Sum256(b)
		m.Files = append(m.Files, FileEntry{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	out, err := json.MarshalIndent(m, "", "  ")
	return append(out, '\n'), err
}

// WriteDir writes a complete, sealed attempt directory. Used by the agent for
// its local copy and by tests; the store seals uploads on its own path.
func WriteDir(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for path, b := range files {
		if err := writeSynced(filepath.Join(dir, path), b); err != nil {
			return err
		}
	}
	manifest, err := BuildManifest(files)
	if err != nil {
		return err
	}
	return writeAtomic(dir, ManifestFile, manifest)
}

func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeAtomic is how a manifest comes to exist: completely or not at all. The
// directory is fsynced after the rename so the rename itself survives a crash.
func writeAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func ReadSamples(r io.Reader) ([]Sample, error) {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for line := 1; sc.Scan(); line++ {
		var s Sample
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("samples line %d: %w", line, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}
