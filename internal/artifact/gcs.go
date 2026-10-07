package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// GCSStore keeps runs and binaries in a Cloud Storage bucket, laid out as the
// contract lays out a store: runs/<run_id>/attempt-<n>/<file>, and binaries
// at blobs/<sha256>.
//
// Every object is written with a does-not-exist precondition, so a file, once
// written, never changes. A retry that sends the same bytes finds the object
// already there with the same content and succeeds, which is what makes
// uploads safe to repeat; anything else is refused. The manifest is written
// the same way and last, so it is the seal.
type GCSStore struct {
	Bucket *storage.BucketHandle
	Prefix string
	// Timeout bounds each operation. A store that hangs holds an agent's
	// upload, and the agent's spool, for as long as it hangs.
	Timeout time.Duration
}

func NewGCSStore(ctx context.Context, url string) (*GCSStore, error) {
	rest, ok := strings.CutPrefix(url, "gs://")
	if !ok || rest == "" {
		return nil, fmt.Errorf("artifact: %q is not a gs:// URL", url)
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GCSStore{Bucket: client.Bucket(bucket), Prefix: strings.Trim(prefix, "/"), Timeout: 30 * time.Second}, nil
}

func (s *GCSStore) key(parts ...string) string {
	return path.Join(append([]string{s.Prefix}, parts...)...)
}

func (s *GCSStore) attemptKey(runID string, attempt int, name string) (string, error) {
	if !safeName.MatchString(runID) || attempt < 1 || (name != "" && !safeName.MatchString(name)) {
		return "", fmt.Errorf("artifact: bad run %q attempt %d file %q", runID, attempt, name)
	}
	return s.key("runs", runID, "attempt-"+strconv.Itoa(attempt), name), nil
}

func (s *GCSStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.Timeout)
}

func isPrecondition(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed
}

// writeOnce writes b at key unless an object is already there, and treats an
// existing object with identical content as success.
func (s *GCSStore) writeOnce(ctx context.Context, key string, b []byte) error {
	w := s.Bucket.Object(key).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(b); err != nil {
		w.Close()
		return err
	}
	err := w.Close()
	if err == nil {
		return nil
	}
	if !isPrecondition(err) {
		return err
	}
	existing, rerr := s.read(ctx, key)
	if rerr != nil {
		return rerr
	}
	if bytes.Equal(existing, b) {
		return nil
	}
	return ErrSealed
}

func (s *GCSStore) read(ctx context.Context, key string) ([]byte, error) {
	r, err := s.Bucket.Object(key).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (s *GCSStore) PutFile(runID string, attempt int, name, digest string, body io.Reader) error {
	if name == ManifestFile {
		return fmt.Errorf("artifact: the manifest is written by Seal")
	}
	if !digestHex.MatchString(digest) {
		return fmt.Errorf("artifact: bad digest")
	}
	key, err := s.attemptKey(runID, attempt, name)
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
	ctx, cancel := s.ctx()
	defer cancel()
	return s.writeOnce(ctx, key, b)
}

// Seal checks every listed object's size and SHA-256, and that nothing
// unlisted sits beside them, before writing the manifest. Cloud Storage keeps
// MD5 and CRC32C, not SHA-256, so each listed object is read back and hashed;
// run files are small enough that this costs little and proves the most.
func (s *GCSStore) Seal(runID string, attempt int, manifest []byte) error {
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil || m.SchemaVersion != ManifestSchema {
		return fmt.Errorf("artifact: manifest unreadable")
	}
	dir, err := s.attemptKey(runID, attempt, "")
	if err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()
	mkey := path.Join(dir, ManifestFile)
	if existing, err := s.read(ctx, mkey); err == nil {
		if bytes.Equal(existing, manifest) {
			return nil
		}
		return ErrSealed
	}
	listed := map[string]bool{}
	for _, f := range m.Files {
		if !safeName.MatchString(f.Path) {
			return fmt.Errorf("artifact: bad file name %q", f.Path)
		}
		b, err := s.read(ctx, path.Join(dir, f.Path))
		if err != nil {
			return fmt.Errorf("artifact: %s not uploaded", f.Path)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
			return fmt.Errorf("%w: %s", ErrDigest, f.Path)
		}
		listed[f.Path] = true
	}
	it := s.Bucket.Objects(ctx, &storage.Query{Prefix: dir + "/"})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(attrs.Name, dir+"/")
		if !listed[name] {
			return fmt.Errorf("artifact: %s uploaded but not listed", name)
		}
	}
	return s.writeOnce(ctx, mkey, manifest)
}

func (s *GCSStore) PutBlob(body io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	ctx, cancel := s.ctx()
	defer cancel()
	return digest, s.writeOnce(ctx, s.key("blobs", digest), b)
}

func (s *GCSStore) OpenBlob(digest string) (io.ReadCloser, error) {
	if !digestHex.MatchString(digest) {
		return nil, fmt.Errorf("artifact: bad digest")
	}
	return s.Bucket.Object(s.key("blobs", digest)).NewReader(context.Background())
}
