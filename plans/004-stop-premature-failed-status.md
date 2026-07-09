# Plan 004: Stop marking archive items `failed` on retryable attempts

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/workers/archive_worker.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: 001
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

River retries a failed archive job up to `MaxAttempts` (3). The intent, per
`ArchiveWorker.Work`, is that an item is only marked `failed` **permanently on
the final attempt** (`job.Attempt >= job.MaxAttempts`). But `processArchiveJob`
writes `status = "failed"` unconditionally on **every** failed attempt (both the
archiver-error and save-error paths). Consequences:

1. During normal retry backoff, the item shows `failed` even though a retry is
   scheduled; `ServeArchive` then returns 404/`failed` for content that is about
   to succeed.
2. `RetryAllFailedJobs` (the admin "retry failed" action) queries
   `status = 'failed'` and can match an item that **still has a live retryable
   River job**, enqueuing a second job for the same work — duplicate concurrent
   archiving and racing status writes.

The fix: delete the two intermediate `failed` writes. `Work` already performs the
correct terminal transition on the final attempt, so retryable attempts should
simply leave the item in `processing`.

## Current state

`internal/workers/archive_worker.go`.

`Work` already handles terminal failure correctly (lines 81–97):
```go
	if err != nil {
		logger.Error("Job processing failed", "error", err)
		// On the final attempt, mark as failed permanently and append a clear message
		if job.Attempt >= job.MaxAttempts {
			_ = w.db.Model(&item).Updates(map[string]interface{}{
				"status":     "failed",
				"updated_at": time.Now(),
			}).Error
			_ = utils.AppendArchiveItemLog(w.db, item.ID, job.Attempt, fmt.Sprintf("\n\nFinal attempt failed after %d tries: %v", job.MaxAttempts, err))
			/* ... */
		}
		// Let River retry (if any attempts left)
		return err
	}
```

The bug is in `processArchiveJob` (lines 135–151) — two unconditional `failed`
writes:
```go
	data, ext, _, bundle, err := arch.Archive(ctx, jobArgs.URL, dbLogWriter, db, item.ID)

	if bundle != nil {
		defer bundle.Cleanup()
	}

	if err != nil {
		slog.Error("Archive operation failed", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		db.Model(item).Update("status", "failed")   // <-- remove
		return err
	}

	// ... saveArchiveData ...
	err = saveArchiveData(data, key, ext, storage, db, item)
	if err != nil {
		slog.Error("Failed to save archive data", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		fmt.Fprintf(dbLogWriter, "\nFailed to save archive data: %v\n", err)
		db.Model(item).Update("status", "failed")   // <-- remove
		return err
	}
```

For context on the double-enqueue risk, `RetryAllFailedJobs`
(`internal/handlers/admin.go:154`) selects `db.Where("status = 'failed'")` and
enqueues a new job per matched item.

`Work` sets the item to `processing` at the start of each attempt (lines 72–75),
so after removing the intermediate writes, a retryable failure leaves the item in
`processing` until the next attempt re-sets it (idempotent) or the final attempt
marks it `failed`.

## Commands you will need

| Purpose      | Command                                                            | Expected   |
|--------------|-------------------------------------------------------------------|------------|
| Build        | `go build ./...`                                                  | exit 0     |
| Vet          | `go vet ./...`                                                    | exit 0     |
| Unit test    | `go test ./internal/workers/ -run TestProcessArchiveJob -count=1` | `ok`       |
| Format check | `gofmt -l internal/workers/archive_worker.go`                    | no output  |
| Tests (all)  | `go test ./... -count=1`                                          | no `FAIL`  |

## Scope

**In scope**:
- `internal/workers/archive_worker.go` (remove two lines)
- `internal/workers/archive_worker_test.go` (create — a focused test for this fix;
  Plan 014 adds broader worker tests and may extend this file)

**Out of scope**:
- `Work`'s final-attempt block — leave it exactly as is; it is the correct
  terminal transition.
- `RetryAllFailedJobs` — its query is fine once items stop being falsely `failed`.
  Do not change it here.
- `saveArchiveData`'s success write (`status="completed"`) — leave it.

## Git workflow

- Branch: `advisor/004-no-premature-failed`
- Commit message: `Keep retryable archive attempts in processing, not failed`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Remove the two intermediate `failed` writes

In `processArchiveJob`, delete the line `db.Model(item).Update("status", "failed")`
in the archiver-error branch (currently line 137) and the identical line in the
save-error branch (currently line 149). Keep the `slog.Error` logging, the
`fmt.Fprintf(dbLogWriter, ...)` line, and the `return err` in each branch.

After the edit, the archiver-error branch reads:
```go
	if err != nil {
		slog.Error("Archive operation failed", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		return err
	}
```
and the save-error branch reads:
```go
	if err != nil {
		slog.Error("Failed to save archive data", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		fmt.Fprintf(dbLogWriter, "\nFailed to save archive data: %v\n", err)
		return err
	}
```

**Verify**: `grep -n 'Update("status", "failed")' internal/workers/archive_worker.go`
→ returns nothing (no matches). `go build ./...` → exit 0.

### Step 2: Add a focused regression test

Create `internal/workers/archive_worker_test.go`. It calls `processArchiveJob`
directly with a stub archiver that returns an error, and asserts the item is NOT
left in `failed` (it should remain `processing`, the status `Work` set before the
attempt). Uses in-memory sqlite (same harness style as
`internal/handlers/logs_test.go`) and `storage.MemoryStorage`.

```go
package workers

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type stubArchiver struct{ err error }

func (s stubArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *archivers.PWBundle, error) {
	return nil, "", "", nil, s.err
}

func newWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestProcessArchiveJobDoesNotMarkFailedOnRetryableError verifies a failed
// attempt leaves the item in "processing", not "failed" (regression: premature
// failed status during retry backoff).
func TestProcessArchiveJobDoesNotMarkFailedOnRetryableError(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "abc12"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": stubArchiver{err: errors.New("boom")}}
	args := ArchiveJobArgs{ShortID: "abc12", Type: "mhtml", URL: "https://example.com"}

	err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m)
	if err == nil {
		t.Fatal("expected an error from the stub archiver")
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status == "failed" {
		t.Fatalf("item was marked failed on a retryable attempt; want processing, got %q", got.Status)
	}
}
```

**Verify**:
`go test ./internal/workers/ -run TestProcessArchiveJob -count=1` → `ok`.

## Test plan

- New test asserts a retryable archiver error does NOT set `status=failed`.
- Harness pattern: model after `internal/handlers/logs_test.go:21-31`
  (in-memory sqlite `AutoMigrate`) plus `storage.NewMemoryStorage()`.
- Full suite: `go test ./... -count=1` → no `FAIL`.
- Note: this file also becomes the home for Plan 014's broader worker tests; if
  014 runs first and the file exists, add the test above rather than recreating
  the file, and reuse its `newWorkerTestDB`/`stubArchiver` helpers.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n 'Update("status", "failed")' internal/workers/archive_worker.go`
      returns nothing
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/workers/ -run TestProcessArchiveJob -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/workers/archive_worker.go internal/workers/archive_worker_test.go`
      prints nothing
- [ ] `git status` shows only `archive_worker.go` modified and
      `archive_worker_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- The two `db.Model(item).Update("status", "failed")` lines are not present as in
  the "Current state" excerpt (the file drifted).
- Removing them leaves an unused variable or import (it should not — only the two
  statements are deleted).
- The `Archiver` interface signature differs from the `stubArchiver` method above,
  so the test will not compile — report the real interface from
  `internal/archivers/archiver.go`.

## Maintenance notes

- After this change, a persistently-failing item sits in `processing` between
  attempts and only becomes `failed` on the final attempt. If someone later adds
  a dashboard that treats "processing for a long time" as stuck, account for
  retry backoff windows (River's `RescueStuckJobsAfter` is configured in
  `cmd/main.go`).
- Reviewer: confirm `Work` still marks terminal failure on the final attempt
  (that block is untouched) — otherwise items could never reach `failed`.
