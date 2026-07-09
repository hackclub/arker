# Plan 010: Remove a half-unpacked git cache dir on failure (stop serving corrupt repos forever)

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/handlers/git.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: MED
- **Depends on**: 001
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

The git HTTP backend unpacks an archived repo into `cacheRoot/<shortID>` the first
time it is requested, gated on `os.Stat(targetDir)` being "not exists".
`unpackGit` does `os.MkdirAll(targetDir)` **first**, then writes files. If any
write fails partway (disk full, corrupt tar, truncated read), it returns an error
and the handler responds 500 — but the partially-populated `targetDir` is left in
place. Every subsequent request then sees `targetDir` already exists, **skips
re-unpacking, and serves the incomplete repository indefinitely** (git
clones/fetches against it fail). Only manual cache deletion recovers it. The fix
makes `unpackGit` clean up its own partial output on error, so the next request
re-unpacks.

## Current state

`internal/handlers/git.go` — the cache gate in `GitHandler` (lines 48–59):
```go
	targetDir := filepath.Join(cacheRoot, shortID)
	cacheMutex.Lock()
	_, err := os.Stat(targetDir)
	if os.IsNotExist(err) {
		if err := unpackGit(item.StorageKey, targetDir, storage); err != nil {
			cacheMutex.Unlock()
			log.Printf("Unpack error: %v", err)
			c.Status(http.StatusInternalServerError)
			return
		}
	}
	cacheMutex.Unlock()
```

`unpackGit` (lines 77–117) — creates the dir, then writes, with no cleanup on error:
```go
func unpackGit(key string, targetDir string, storage storage.Storage) error {
	r, err := storage.Reader(key)
	if err != nil {
		return err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	if err = os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// ... writes files under targetDir ...
	}
	return nil
}
```

## Commands you will need

| Purpose      | Command                                                        | Expected   |
|--------------|---------------------------------------------------------------|------------|
| Build        | `go build ./...`                                              | exit 0     |
| Vet          | `go vet ./...`                                                | exit 0     |
| Unit test    | `go test ./internal/handlers/ -run TestUnpackGit -count=1`   | `ok`       |
| Format check | `gofmt -l internal/handlers/git.go`                          | no output  |
| Tests (all)  | `go test ./... -count=1`                                      | no `FAIL`  |

## Scope

**In scope**:
- `internal/handlers/git.go` (make `unpackGit` clean up on error)
- `internal/handlers/git_unpack_test.go` (create)

**Out of scope**:
- The CGI/`git-http-backend` invocation and PATH_INFO handling — separate finding,
  not in this plan.
- The tar-slip containment check — separate finding; do not add it here.
- `GitHandler`'s locking — unchanged.

## Git workflow

- Branch: `advisor/010-git-cache-cleanup-on-error`
- Commit message: `Remove partial git cache dir when unpack fails`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Clean up `targetDir` when unpack fails

Change `unpackGit` to use a named error return and a deferred cleanup that removes
`targetDir` on any error after the directory is created:

```go
func unpackGit(key string, targetDir string, storage storage.Storage) (err error) {
	r, err := storage.Reader(key)
	if err != nil {
		return err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	if err = os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	// If unpacking fails partway, remove the partial directory so the stat-gate in
	// GitHandler re-unpacks on the next request instead of serving a corrupt repo.
	defer func() {
		if err != nil {
			os.RemoveAll(targetDir)
		}
	}()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// ... unchanged body ...
	}
	return nil
}
```

Important: the loop currently declares a **shadowed** `err` via
`hdr, err := tr.Next()`. That inner `err` shadows the named return. To make the
deferred cleanup see the real error, ensure each `return err` inside the loop
returns the value (it does — Go copies it into the named return on `return err`).
This works because `return err` explicitly assigns the named return before the
deferred func runs. Do not change the loop's `:=`; just add the named return and
the deferred cleanup as shown.

**Verify**: `go build ./...` → exit 0; and
`grep -n "os.RemoveAll(targetDir)" internal/handlers/git.go` → returns one line.

### Step 2: Add a regression test

Create `internal/handlers/git_unpack_test.go`. It feeds `unpackGit` a corrupt tar
via `storage.MemoryStorage` and asserts (a) it returns an error and (b) the target
directory does not exist afterward.

```go
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
```

**Verify**:
`go test ./internal/handlers/ -run TestUnpackGit -count=1` → `ok`.

## Test plan

- New test `TestUnpackGitRemovesDirOnCorruptTar` proves the partial dir is removed
  on failure. Uses `storage.MemoryStorage`; no network, no real git.
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "os.RemoveAll(targetDir)" internal/handlers/git.go` returns one line
- [ ] `unpackGit` uses a named return `(err error)`
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/handlers/ -run TestUnpackGit -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/handlers/git.go internal/handlers/git_unpack_test.go` prints nothing
- [ ] `git status` shows only `git.go` modified and `git_unpack_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- `unpackGit` does not match the "Current state" excerpt.
- The test finds the directory is NOT removed even after your edit — this means
  the named-return/`return err` interaction did not propagate the error to the
  deferred func; report it (do not add a second cleanup in GitHandler as a
  workaround without reporting).
- `tar.Reader.Next` returns `io.EOF` (not an error) for the 0xff garbage on this
  Go version, so the test's corrupt input does not error — switch the garbage to a
  truncated-but-plausible tar and report.

## Maintenance notes

- A stronger design unpacks into a sibling temp dir and `os.Rename`s into place
  only on full success (atomic, and never exposes a partial dir even briefly). If
  the git cache grows hot enough to matter, consider that upgrade; the current fix
  is sufficient to end the "corrupt forever" behavior.
- Reviewer: confirm the deferred cleanup only fires on error (the `if err != nil`
  guard), not on success.
