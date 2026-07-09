package handlers

import (
	"os"
	"path/filepath"
	"testing"

	"arker/internal/storage"
)

// TestUnpackGitRemovesDirOnCorruptTar verifies a failed unpack leaves no partial
// cache directory behind (regression: poisoned git cache served forever).
func TestUnpackGitRemovesDirOnCorruptTar(t *testing.T) {
	ms := storage.NewMemoryStorage()
	// Write bytes that are not a valid tar; tar.Reader.Next will error (not EOF).
	w, _ := ms.Writer("bad/key")
	garbage := make([]byte, 512)
	for i := range garbage {
		garbage[i] = 0xff
	}
	w.Write(garbage)
	w.Close()

	targetDir := filepath.Join(t.TempDir(), "repo-cache")
	err := unpackGit("bad/key", targetDir, ms)
	if err == nil {
		t.Fatal("expected unpackGit to fail on corrupt tar")
	}
	if _, statErr := os.Stat(targetDir); !os.IsNotExist(statErr) {
		t.Fatalf("target dir was not cleaned up after failed unpack: statErr=%v", statErr)
	}
}
