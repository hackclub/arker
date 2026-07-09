# Plan 003: Remove the git archiver's clone temp dir after every capture

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/archivers/git.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none (independent of Plan 002, though both touch `git.go` — if
  doing both, land 002 first and re-run the drift check)
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`GitArchiver.Archive` clones the repo into a fresh `os.MkdirTemp` directory and
defines `cleanup := func() { os.RemoveAll(tempDir) }`. But `cleanup()` is invoked
**only on the clone-error path**. On success the archiver streams a tar from
`tempDir` in a goroutine and never removes it. Git archivers return `bundle == nil`,
so the worker's `bundle.Cleanup()` path (`archive_worker.go:131-133`) never covers
it either. The result: **every successful git capture leaks a full bare-repo clone
under the OS temp dir, forever**. On a production git-archiving server this is
unbounded disk growth until the volume fills and archiving (and possibly the host)
fails. The fix is to remove the temp dir once the tar goroutine has finished
reading from it.

## Current state

`internal/archivers/git.go`, the relevant region of `Archive` (lines 70–131):

```go
	tempDir, err := os.MkdirTemp("", "git-archive-")
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create temp directory: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { os.RemoveAll(tempDir) }

	fmt.Fprintf(logWriter, "Cloning repository to: %s\n", tempDir)
	_, err = git.PlainCloneContext(ctx, tempDir, true, &git.CloneOptions{
		URL:      repoURL,
		Progress: logWriter,
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to clone repository: %v\n", err)
		cleanup()                       // <-- only cleanup call today
		return nil, "", "", nil, err
	}
	fmt.Fprintf(logWriter, "Repository cloned successfully\n")

	pr, pw := io.Pipe()

	// Start context-aware tar creation in a goroutine
	go func() {
		defer pw.Close()

		// Check context before starting tar creation
		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during git tar creation\n")
			pw.CloseWithError(ctx.Err())
			return
		default:
		}

		tw := tar.NewWriter(pw)
		defer tw.Close()

		fmt.Fprintf(logWriter, "Creating tar archive...\n")

		done := make(chan error, 1)
		go func() {
			done <- AddDirToTar(tw, tempDir, "")
		}()

		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during git tar creation\n")
			pw.CloseWithError(ctx.Err())
		case err := <-done:
			if err != nil {
				fmt.Fprintf(logWriter, "Failed to create tar archive: %v\n", err)
				pw.CloseWithError(err)
			} else {
				fmt.Fprintf(logWriter, "Git archive completed successfully\n")
			}
		}
	}()

	return pr, ".tar", "application/x-tar", nil, nil
```

Key facts that make the fix safe:
- `io.Pipe()` is unbuffered/synchronous: writes block until the consumer
  (`saveArchiveData`'s `io.Copy`) reads. By the time `AddDirToTar` returns, all
  file bytes have been consumed and every file it opened has been closed
  (`AddDirToTar` opens/copies/closes each file in turn — see `git.go:196-204`).
- Therefore, removing `tempDir` after the goroutine's writers close is safe: no
  file handles remain open and no further reads occur.

## Commands you will need

| Purpose      | Command                                                        | Expected              |
|--------------|---------------------------------------------------------------|-----------------------|
| Build        | `go build ./...`                                              | exit 0                |
| Vet          | `go vet ./...`                                                | exit 0                |
| Unit test    | `go test ./internal/archivers/ -run TestGitArchiveCleansTempDir -count=1` | `ok` |
| Format check | `gofmt -l internal/archivers/git.go`                          | no output             |
| Tests (all)  | `go test ./... -count=1`                                      | no `FAIL`             |

## Scope

**In scope**:
- `internal/archivers/git.go` (edit the goroutine in `Archive`)
- `internal/archivers/git_cleanup_test.go` (create)

**Out of scope**:
- `AddDirToTar` — do not change its behavior.
- `unpackGit` / `handlers/git.go` — separate (Plan 010).
- The clone-error `cleanup()` call at line 84 — leave it; it is correct.

## Git workflow

- Branch: `advisor/003-git-tempdir-cleanup`
- Commit message: `Remove git clone temp dir after tar streaming completes`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Run cleanup when the tar goroutine finishes

Inside the `go func()` that streams the tar, register `cleanup` as the FIRST
deferred call, so (by LIFO ordering) it runs **after** `pw.Close()` — i.e. after
all tar bytes have been written and consumed. Change the top of the goroutine from:

```go
	go func() {
		defer pw.Close()
```
to:
```go
	go func() {
		defer cleanup() // remove the clone temp dir once streaming is done
		defer pw.Close()
```

Do not change anything else in the goroutine. The clone-error path at line 84
already calls `cleanup()` before the goroutine is ever started, so success and
error are now both covered exactly once.

**Verify**: `go build ./...` → exit 0; and
`grep -n "defer cleanup()" internal/archivers/git.go` → returns one line inside
the goroutine.

### Step 2: Add a regression test

Create `internal/archivers/git_cleanup_test.go`. This test does not hit the
network: it invokes `Archive` against a `file://` URL pointing at a real local
git repository created in the test, drains the returned reader, and asserts no
`git-archive-*` temp dirs are left behind.

```go
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
```

**Verify**:
`go test ./internal/archivers/ -run TestGitArchiveCleansTempDir -count=1` → `ok`.

## Test plan

- New test `TestGitArchiveCleansTempDir` asserts the success path leaves no
  `git-archive-*` temp directory. It clones a local `file://` repo (no network).
- It `t.Skip`s if the `git` binary is absent, so it is CI-safe (the CI image from
  Plan 001 has git).
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `grep -n "defer cleanup()" internal/archivers/git.go` returns exactly one
      match, inside the tar goroutine
- [ ] `go test ./internal/archivers/ -run TestGitArchiveCleansTempDir -count=1`
      passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/archivers/git.go internal/archivers/git_cleanup_test.go`
      prints nothing
- [ ] `git status` shows only `git.go` modified and `git_cleanup_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- The `Archive` goroutine does not match the "Current state" excerpt (e.g.
  `defer pw.Close()` is no longer the first line).
- The test fails because temp dirs ARE left behind even after your edit — this
  means the goroutine exits before the consumer finishes reading; report it
  rather than adding sleeps.
- `Archive`'s signature differs from `Archive(ctx, url, logWriter, db, itemID)`
  (the test's call would not compile) — report the actual signature.

## Maintenance notes

- If a future change makes git archiving return a `*PWBundle` (it does not today),
  make sure temp-dir cleanup is not duplicated with `bundle.Cleanup()`.
- Reviewers: confirm cleanup runs after `pw.Close()` (LIFO), not before — running
  it before would delete files mid-stream and corrupt the tar.
