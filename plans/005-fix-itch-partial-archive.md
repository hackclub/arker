# Plan 005: Fail itch archives that can't add all files (stop storing partial ZIPs as "completed")

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/archivers/itch.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

When the itch archiver zips a downloaded game, `addDirectoryToZip` **logs** any
per-file or per-subdirectory failure as a `Warning:` and then returns `nil`. So
`addGameToZip` always returns `nil`, the streaming goroutine never calls
`pipeWriter.CloseWithError`, and `saveArchiveData`'s `io.Copy` succeeds against a
ZIP that is silently missing files. The item is marked `completed`. The result is
**invisible data loss**: a partially-readable game download is stored and reported
as a good archive, with no error and no retry. The fix is to propagate the first
file/dir error so the pipe closes with an error and the job fails (and River
retries) instead of persisting a truncated archive.

## Current state

`internal/archivers/itch.go`.

The streaming goroutine (lines 109–124) only errors the pipe if `addGameToZip`
returns non-nil:
```go
	go func() {
		defer pipeWriter.Close()
		defer os.RemoveAll(tmpDir)

		zipWriter := zip.NewWriter(pipeWriter)
		defer zipWriter.Close()

		if err := addGameToZip(zipWriter, gameDir, metadata, logWriter); err != nil {
			fmt.Fprintf(logWriter, "Error adding files to ZIP: %v\n", err)
			pipeWriter.CloseWithError(err)
			return
		}

		fmt.Fprintf(logWriter, "Successfully created ZIP archive\n")
	}()
```

`addGameToZip` just delegates (lines 275–279):
```go
func addGameToZip(zipWriter *zip.Writer, gameDir string, metadata *ItchMetadata, logWriter io.Writer) error {
	return addDirectoryToZip(zipWriter, gameDir, "", logWriter)
}
```

The bug is in `addDirectoryToZip` (lines 281–309) — errors are logged and
swallowed:
```go
func addDirectoryToZip(zipWriter *zip.Writer, sourceDir, zipPrefix string, logWriter io.Writer) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		sourcePath := filepath.Join(sourceDir, entry.Name())
		zipPath := entry.Name()
		if zipPrefix != "" {
			zipPath = filepath.Join(zipPrefix, entry.Name())
		}

		if entry.IsDir() {
			if err := addDirectoryToZip(zipWriter, sourcePath, zipPath, logWriter); err != nil {
				fmt.Fprintf(logWriter, "Warning: failed to add directory %s: %v\n", zipPath, err)   // <-- swallows
			}
		} else {
			if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
				fmt.Fprintf(logWriter, "Warning: failed to add file %s: %v\n", zipPath, err)         // <-- swallows
			}
		}
	}

	return nil
}
```

`addFileFromDiskToZip` (lines 311–337) already returns real errors from
`os.Open`/`io.Copy`; the problem is only that the caller drops them.

## Commands you will need

| Purpose      | Command                                                             | Expected  |
|--------------|--------------------------------------------------------------------|-----------|
| Build        | `go build ./...`                                                   | exit 0    |
| Vet          | `go vet ./...`                                                     | exit 0    |
| Unit test    | `go test ./internal/archivers/ -run TestAddDirectoryToZip -count=1` | `ok`     |
| Format check | `gofmt -l internal/archivers/itch.go`                             | no output |
| Tests (all)  | `go test ./... -count=1`                                           | no `FAIL` |

## Scope

**In scope**:
- `internal/archivers/itch.go` (edit `addDirectoryToZip`)
- `internal/archivers/itch_zip_test.go` (create)

**Out of scope**:
- `addFileFromDiskToZip` — leave as is; it already returns errors.
- The streaming goroutine — it already calls `CloseWithError` on a non-nil error;
  no change needed once `addDirectoryToZip` propagates.
- `parseItchMetadata`, `findGameDirectory`, the itch download path — unrelated.

## Git workflow

- Branch: `advisor/005-itch-partial-archive`
- Commit message: `Fail itch archive when a game file cannot be zipped`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Propagate the first file/dir error

Rewrite the loop body of `addDirectoryToZip` so a file or subdirectory failure is
returned instead of logged-and-continued. Keep the log line for operator
visibility, but then return the error:

```go
	for _, entry := range entries {
		sourcePath := filepath.Join(sourceDir, entry.Name())
		zipPath := entry.Name()
		if zipPrefix != "" {
			zipPath = filepath.Join(zipPrefix, entry.Name())
		}

		if entry.IsDir() {
			if err := addDirectoryToZip(zipWriter, sourcePath, zipPath, logWriter); err != nil {
				fmt.Fprintf(logWriter, "Failed to add directory %s: %v\n", zipPath, err)
				return fmt.Errorf("add directory %s: %w", zipPath, err)
			}
		} else {
			if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
				fmt.Fprintf(logWriter, "Failed to add file %s: %v\n", zipPath, err)
				return fmt.Errorf("add file %s: %w", zipPath, err)
			}
		}
	}

	return nil
```

**Verify**: `go build ./...` → exit 0; and
`grep -n "Warning: failed to add" internal/archivers/itch.go` → returns nothing.

### Step 2: Add a regression test

Create `internal/archivers/itch_zip_test.go`. It builds a temp directory
containing one good file and one **dangling symlink** (whose target does not
exist), so `os.Open` inside `addFileFromDiskToZip` fails with ENOENT even when the
test runs as root. It asserts `addGameToZip` returns a non-nil error.

```go
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
```

**Verify**:
`go test ./internal/archivers/ -run TestAddDirectoryToZip -count=1` → `ok`.

## Test plan

- New test asserts an unreadable entry causes `addGameToZip` to return an error.
- No network/browser; uses only a temp dir and a dangling symlink.
- If `ItchMetadata` has required fields beyond `Title`, adjust the literal to
  compile (see the struct definition in `itch.go`); a zero-value `&ItchMetadata{}`
  is acceptable if `addGameToZip` does not dereference other fields.
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "Warning: failed to add" internal/archivers/itch.go` returns nothing
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/archivers/ -run TestAddDirectoryToZip -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/archivers/itch.go internal/archivers/itch_zip_test.go`
      prints nothing
- [ ] `git status` shows only `itch.go` modified and `itch_zip_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- `addDirectoryToZip` does not match the "Current state" excerpt.
- The test does not compile because `ItchMetadata` requires fields you cannot
  infer from the struct — report the struct definition.
- After the fix, existing itch tests (if any) fail because they relied on
  partial-archive tolerance — report which and stop; do not weaken the fix.

## Maintenance notes

- This makes genuinely-partial itch downloads fail and retry, which is the
  intended behavior. If a specific game legitimately contains unreadable entries
  (e.g. broken symlinks shipped by the author), that will now fail the archive;
  if that becomes a real problem, add a narrowly-scoped allowlist rather than
  reverting to swallow-all.
- Reviewer: confirm the streaming goroutine still calls
  `pipeWriter.CloseWithError(err)` on the propagated error (it does, unchanged).
