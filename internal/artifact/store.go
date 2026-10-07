package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// ErrSealed is returned for any write that would change a sealed attempt. A
// retry that sends identical bytes is not a change and succeeds, which is
// what makes uploads safe to repeat after a lost acknowledgement.
var ErrSealed = errors.New("artifact: attempt is sealed")

// ErrDigest means the bytes received are not the bytes the sender hashed.
var ErrDigest = errors.New("artifact: digest mismatch")

var (
	safeName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	digestHex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Store is where sealed runs and benchmark binaries live. The filesystem
// store serves a single machine and the tests; the GCS store serves a fleet.
// Both keep the one property the contract depends on: a manifest becomes
// visible only after every file it lists is in place and matches.
type Store interface {
	PutFile(runID string, attempt int, name, digest string, body io.Reader) error
	Seal(runID string, attempt int, manifest []byte) error
	PutBlob(body io.Reader) (string, error)
	OpenBlob(digest string) (io.ReadCloser, error)
}

// FSStore is a directory standing in for an object store bucket. It keeps the
// one property the contract depends on: the manifest becomes visible
// atomically and only after every file it lists is in place.
type FSStore struct {
	Root string
	mu   sync.Mutex
}

func (s *FSStore) dir(runID string, attempt int) (string, error) {
	if !safeName.MatchString(runID) || attempt < 1 {
		return "", fmt.Errorf("artifact: bad run %q attempt %d", runID, attempt)
	}
	return AttemptDir(s.Root, runID, attempt), nil
}

func (s *FSStore) PutFile(runID string, attempt int, name, digest string, body io.Reader) error {
	if !safeName.MatchString(name) || name == ManifestFile {
		return fmt.Errorf("artifact: bad file name %q", name)
	}
	if !digestHex.MatchString(digest) {
		return fmt.Errorf("artifact: bad digest")
	}
	dir, err := s.dir(runID, attempt)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return ErrDigest
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(dir, name)
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
		existing, err := os.ReadFile(path)
		if err == nil && bytes.Equal(existing, b) {
			return nil
		}
		return ErrSealed
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeSynced(path, b)
}

// Seal checks every listed file against its digest before making the
// manifest visible. A file present but not listed is rejected too: a sealed
// directory has to mean exactly what its manifest says.
func (s *FSStore) Seal(runID string, attempt int, manifest []byte) error {
	dir, err := s.dir(runID, attempt)
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil || m.SchemaVersion != ManifestSchema {
		return fmt.Errorf("artifact: manifest unreadable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := os.ReadFile(filepath.Join(dir, ManifestFile)); err == nil {
		if bytes.Equal(existing, manifest) {
			return nil
		}
		return ErrSealed
	}
	listed := map[string]bool{}
	for _, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			return fmt.Errorf("artifact: %s not uploaded", f.Path)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
			return fmt.Errorf("%w: %s", ErrDigest, f.Path)
		}
		listed[f.Path] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !listed[e.Name()] && e.Name()[0] != '.' {
			return fmt.Errorf("artifact: %s uploaded but not listed", e.Name())
		}
	}
	return writeAtomic(dir, ManifestFile, manifest)
}

func (s *FSStore) PutBlob(body io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(s.Root, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, digest)); err == nil {
		return digest, nil
	}
	return digest, writeAtomic(dir, digest, b)
}

func (s *FSStore) OpenBlob(digest string) (io.ReadCloser, error) {
	if !digestHex.MatchString(digest) {
		return nil, fmt.Errorf("artifact: bad digest")
	}
	return os.Open(filepath.Join(s.Root, "blobs", digest))
}
