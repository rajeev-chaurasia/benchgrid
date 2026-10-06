package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f, _ := OpenFenceStore(dir)
	if err := f.Advance(108); err != nil {
		t.Fatal(err)
	}
	if err := f.Advance(108); err == nil {
		t.Error("advanced to an equal token")
	}
	g, err := OpenFenceStore(dir)
	if err != nil || g.High() != 108 {
		t.Fatalf("restart forgot the mark: %d %v", g.High(), err)
	}
}

func TestCorruptFenceRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fence"), []byte("10x"), 0o644)
	if _, err := OpenFenceStore(dir); err == nil {
		t.Error("started with a corrupt fence mark")
	}
}
