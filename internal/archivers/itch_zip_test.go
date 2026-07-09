package archivers

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAddDirectoryToZipPropagatesFileError verifies a file that cannot be read
// fails the whole archive instead of being silently skipped (regression: partial
// itch ZIP stored as completed).
func TestAddDirectoryToZipPropagatesFileError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dangling symlink: os.Open follows it and fails with "no such file".
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "broken")); err != nil {
		t.Fatal(err)
	}

	zw := zip.NewWriter(io.Discard)
	defer zw.Close()

	if err := addGameToZip(zw, dir, &ItchMetadata{Title: "t"}, io.Discard); err == nil {
		t.Fatal("expected addGameToZip to fail when a file cannot be added, got nil")
	}
}
