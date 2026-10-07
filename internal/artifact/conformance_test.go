package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
)

// stores returns every Store implementation, each empty, so the same contract
// is checked against all of them. A real bucket joins the list when
// BENCHGRID_TEST_GCS names one, because a fake can agree with this code and
// still disagree with Cloud Storage.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "bench"})
	out := map[string]Store{
		"fs":  &FSStore{Root: t.TempDir()},
		"gcs": &GCSStore{Bucket: srv.Client().Bucket("bench"), Prefix: "t", Timeout: 10e9},
	}
	if url := os.Getenv("BENCHGRID_TEST_GCS"); url != "" {
		g, err := NewGCSStore(context.Background(), url+"/conformance/"+t.Name())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { deletePrefix(t, g) })
		out["real-gcs"] = g
	}
	return out
}

func deletePrefix(t *testing.T, g *GCSStore) {
	ctx := context.Background()
	it := g.Bucket.Objects(ctx, &storage.Query{Prefix: g.Prefix + "/"})
	for {
		a, err := it.Next()
		if err != nil {
			return
		}
		g.Bucket.Object(a.Name).Delete(ctx)
	}
}

func TestStoresAgree(t *testing.T) {
	run, samples := fixture(t)
	rj, sj, _ := Encode(run, samples)
	manifest, _ := BuildManifest(map[string][]byte{RunFile: rj, SamplesFile: sj})

	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := st.PutFile("exp_c", 1, RunFile, digest(rj), bytes.NewReader(rj)); err != nil {
				t.Fatal(err)
			}
			if err := st.PutFile("exp_c", 1, RunFile, digest(rj), bytes.NewReader(rj)); err != nil {
				t.Fatalf("an identical retry was refused: %v", err)
			}
			if err := st.Seal("exp_c", 1, manifest); err == nil {
				t.Fatal("sealed with a listed file missing")
			}
			if err := st.PutFile("exp_c", 1, SamplesFile, digest(rj), bytes.NewReader(sj)); !errors.Is(err, ErrDigest) {
				t.Fatalf("accepted a body that does not match its digest: %v", err)
			}
			if err := st.PutFile("exp_c", 1, SamplesFile, digest(sj), bytes.NewReader(sj)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := st.Seal("exp_c", 1, manifest); err != nil {
					t.Fatalf("seal %d: %v", i, err)
				}
			}
			other := append([]byte(nil), sj...)
			other[0] = ' '
			if err := st.PutFile("exp_c", 1, SamplesFile, digest(other), bytes.NewReader(other)); !errors.Is(err, ErrSealed) {
				t.Errorf("changed a sealed file: %v", err)
			}
			changed := bytes.Replace(manifest, []byte(`"size"`), []byte(` "size"`), 1)
			if err := st.Seal("exp_c", 1, changed); !errors.Is(err, ErrSealed) {
				t.Errorf("resealed with a different manifest: %v", err)
			}

			if err := st.PutFile("exp_x", 1, RunFile, digest(rj), bytes.NewReader(rj)); err != nil {
				t.Fatal(err)
			}
			if err := st.PutFile("exp_x", 1, "extra.txt", digest([]byte("x")), bytes.NewReader([]byte("x"))); err != nil {
				t.Fatal(err)
			}
			st.PutFile("exp_x", 1, SamplesFile, digest(sj), bytes.NewReader(sj))
			if err := st.Seal("exp_x", 1, manifest); err == nil {
				t.Error("sealed with an unlisted file beside the listed ones")
			}

			if err := st.PutFile("../x", 1, RunFile, digest(rj), bytes.NewReader(rj)); err == nil {
				t.Error("accepted a path traversal run id")
			}

			d, err := st.PutBlob(bytes.NewReader([]byte("binary")))
			if err != nil {
				t.Fatal(err)
			}
			if d2, err := st.PutBlob(bytes.NewReader([]byte("binary"))); err != nil || d2 != d {
				t.Errorf("blob upload not idempotent: %v", err)
			}
			rc, err := st.OpenBlob(d)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "binary" {
				t.Errorf("blob read back as %q", b)
			}
		})
	}
}
