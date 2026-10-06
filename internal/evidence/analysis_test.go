package evidence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverlapPairs(t *testing.T) {
	spans := []Span{
		{"r1", "a", 0, 10},
		{"r1", "b", 10, 20}, // touches a at 10, half open: no overlap
		{"r1", "c", 15, 25}, // overlaps b
		{"r1", "c", 16, 17}, // same owner as c, overlaps b only
		{"r2", "d", 0, 100},
		{"r2", "d", 50, 60}, // same owner: never counted
		{"r3", "e", 0, 10},
		{"r1", "f", 5, 6}, // overlaps a only
	}
	if got := OverlapPairs(spans); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestPeakConcurrency(t *testing.T) {
	spans := []Span{{"", "", 0, 10}, {"", "", 5, 15}, {"", "", 10, 20}, {"", "", 12, 13}}
	if got := PeakConcurrency(spans); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestManifestCatchesEditAndAddition(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.json"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.json"), []byte("2"), 0o644)
	if err := WriteManifest(dir); err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "sub", "b.json"), []byte("3"), 0o644)
	if VerifyManifest(dir) == nil {
		t.Error("edit not caught")
	}
	os.WriteFile(filepath.Join(dir, "sub", "b.json"), []byte("2"), 0o644)
	os.WriteFile(filepath.Join(dir, "c.json"), []byte("4"), 0o644)
	if VerifyManifest(dir) == nil {
		t.Error("addition not caught")
	}
}
