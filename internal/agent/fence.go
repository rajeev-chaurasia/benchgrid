package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FenceStore is the rig's memory of the highest lease token it has acted on.
// It is persisted before the agent acts on a new token. Without that, an agent
// restart would forget the mark, and a frozen scheduler resuming afterwards
// with an older token would be accepted.
type FenceStore struct {
	path string
	mu   sync.Mutex
	high int64
}

func OpenFenceStore(dir string) (*FenceStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fs := &FenceStore{path: filepath.Join(dir, "fence")}
	b, err := os.ReadFile(fs.path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, err
	default:
		fs.high, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			// A corrupt mark cannot be guessed at. Refusing to start is the only
			// answer that cannot accept a stale token.
			return nil, fmt.Errorf("fence store %s is corrupt: %w", fs.path, err)
		}
	}
	return fs, nil
}

func (f *FenceStore) High() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.high
}

// Advance records a strictly higher token durably. It returns an error rather
// than advancing in memory only, because an unpersisted mark is the bug this
// type exists to prevent.
func (f *FenceStore) Advance(token int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token <= f.high {
		return fmt.Errorf("fence %d does not advance %d", token, f.high)
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".fence-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strconv.FormatInt(token, 10) + "\n"); err != nil {
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
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	f.high = token
	return nil
}
