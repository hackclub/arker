package archivers

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitArchiveCleansTempDir verifies the clone temp dir is removed after a
// successful archive (regression for the temp-dir leak).
func TestGitArchiveCleansTempDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	// Create a tiny local git repo to clone via file://.
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.email", "t@t.test")
	runGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "--quiet", "-m", "init")

	before := countTempCloneDirs(t)

	a := &GitArchiver{}
	r, _, _, _, err := a.Archive(context.Background(), "file://"+repo, io.Discard, nil, 0)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("drain: %v", err)
	}

	after := countTempCloneDirs(t)
	if after > before {
		t.Fatalf("git-archive temp dir leaked: before=%d after=%d", before, after)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func countTempCloneDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "git-archive-") {
			n++
		}
	}
	return n
}
