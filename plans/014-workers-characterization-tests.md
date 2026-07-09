# Plan 014: Add characterization tests for the archive worker's save/status logic

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/workers/archive_worker.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpts against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: LOW
- **Depends on**: 001; and 004 (it creates the shared test harness in
  `internal/workers/archive_worker_test.go` — reuse it). Land 004 and 005 before
  this so the tested behavior reflects the fixed logic.
- **Category**: tests
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`internal/workers` — the archive job engine — has **0% test coverage** despite the
worst recent bug history in the repo (queue deadlock, stuck jobs, duplicate
storage keys). This plan adds characterization tests for the parts that are
testable without River or a live Postgres: the storage-key nonce, and the
save/status transition in `saveArchiveData` / `processArchiveJob`. These lock in
the current correct behavior so future changes can't silently strand jobs or
reuse storage keys. (The River-specific `Work` retry accounting and `QueueCapture`
enqueue path need a Postgres+River harness; they are explicitly deferred — see
Maintenance notes.)

## Current state

`internal/workers/archive_worker.go`:

`uploadNonce` (lines 163–169) — must yield a unique suffix per call:
```go
func uploadNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
```

`saveArchiveData` (lines 172–208) — writes to storage and marks the item
completed with key/extension/size:
```go
func saveArchiveData(data io.Reader, key, ext string, storage storage.Storage, db *gorm.DB, item *models.ArchiveItem) error {
	w, err := storage.Writer(key)
	// ... io.Copy(w, data), close reader if Closer, close writer ...
	fileSize, err := storage.Size(key)
	// ...
	return db.Model(item).Updates(map[string]interface{}{
		"status":      "completed",
		"storage_key": key,
		"extension":   ext,
		"file_size":   fileSize,
	}).Error
}
```

`processArchiveJob` (lines 104–159) success path builds the key as
`fmt.Sprintf("%s/%s-%s%s", jobArgs.ShortID, jobArgs.Type, uploadNonce(), ext)` and
calls `saveArchiveData`.

The `Archiver` interface (`internal/archivers/archiver.go:10-12`):
```go
type Archiver interface {
	Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (data io.Reader, extension string, contentType string, bundle *PWBundle, err error)
}
```

Test-harness pattern to follow: `internal/handlers/logs_test.go:21-31`
(in-memory sqlite via `gorm.io/driver/sqlite`, `AutoMigrate` the models) and
`internal/storage/memory_storage.go` (`storage.NewMemoryStorage()`).

Plan 004 adds `internal/workers/archive_worker_test.go` containing
`newWorkerTestDB(t)` and a `stubArchiver` (returns an error). This plan reuses
`newWorkerTestDB` and adds a **new** stub that returns data.

## Commands you will need

| Purpose      | Command                                                | Expected   |
|--------------|--------------------------------------------------------|------------|
| Build        | `go build ./...`                                       | exit 0     |
| Vet          | `go vet ./...`                                          | exit 0     |
| Package test | `go test ./internal/workers/ -count=1`                 | `ok`       |
| Coverage     | `go test ./internal/workers/ -cover -count=1`          | prints a non-zero coverage % |
| Format check | `gofmt -l internal/workers/`                           | no output  |
| Tests (all)  | `go test ./... -count=1`                               | no `FAIL`  |

## Scope

**In scope**:
- `internal/workers/archive_worker_test.go` (extend the file created by Plan 004;
  if Plan 004 has not landed, create it and include `newWorkerTestDB` +
  `stubArchiver` from that plan first)

**Out of scope**:
- `internal/workers/queue.go` `QueueCapture` and `ArchiveWorker.Work` — these need
  River + Postgres; deferred to a DSN-gated integration test (Maintenance notes).
- `internal/workers/cleanup_worker.go` — Postgres-only SQL; deferred.
- Any production (non-test) file. This plan adds tests only.

## Git workflow

- Branch: `advisor/014-workers-tests`
- Commit message: `Add characterization tests for archive worker save path`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Ensure the shared harness exists

If `internal/workers/archive_worker_test.go` already exists (Plan 004 landed),
confirm it defines `newWorkerTestDB(t *testing.T) *gorm.DB`. If it does not exist,
create it with the harness from Plan 004 (the `newWorkerTestDB` helper and the
error-returning `stubArchiver`) before continuing.

**Verify**: `grep -n "func newWorkerTestDB" internal/workers/archive_worker_test.go`
→ returns one line.

### Step 2: Add the tests

Append to `internal/workers/archive_worker_test.go`:

```go
import (
	"bytes"       // add to the existing import block if not present
	"io"          // add if not present
)

// dataArchiver is a stub that returns fixed content and no error.
type dataArchiver struct{ payload []byte }

func (d dataArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *archivers.PWBundle, error) {
	return bytes.NewReader(d.payload), ".mhtml", "application/x-mhtml", nil, nil
}

func TestUploadNonceIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		n := uploadNonce()
		if n == "" {
			t.Fatal("uploadNonce returned empty string")
		}
		if seen[n] {
			t.Fatalf("uploadNonce collision: %q", n)
		}
		seen[n] = true
	}
}

func TestSaveArchiveDataMarksCompleted(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv001"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	payload := []byte("hello-archive")
	key := "sv001/mhtml-deadbeef.mhtml"
	if err := saveArchiveData(bytes.NewReader(payload), key, ".mhtml", store, db, &item); err != nil {
		t.Fatalf("saveArchiveData: %v", err)
	}

	// Storage got the bytes.
	if size, err := store.Size(key); err != nil || size != int64(len(payload)) {
		t.Fatalf("stored size = %d, err %v; want %d", size, err, len(payload))
	}
	// Item is completed with the right metadata.
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey != key || got.Extension != ".mhtml" || got.FileSize != int64(len(payload)) {
		t.Fatalf("item not finalized correctly: %+v", got)
	}
}

func TestProcessArchiveJobSuccessCompletes(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv002"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": dataArchiver{payload: []byte("body")}}
	args := ArchiveJobArgs{ShortID: "sv002", Type: "mhtml", URL: "https://example.com"}

	if err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.StorageKey == "" {
		t.Fatal("storage key not set on completion")
	}
}
```

Adjust the import block so `bytes`, `io`, `context`, `time`,
`arker/internal/archivers`, `arker/internal/models`, and `arker/internal/storage`
are all imported (some are already there from Plan 004). `gofmt`/`go vet` will flag
missing or unused imports.

**Verify**: `go test ./internal/workers/ -count=1` → `ok`.

## Test plan

- `TestUploadNonceIsUnique`: 1000 calls, no empty result, no collision.
- `TestSaveArchiveDataMarksCompleted`: storage receives the bytes and the item is
  finalized (`completed`, key, extension, size).
- `TestProcessArchiveJobSuccessCompletes`: the success path through
  `processArchiveJob` marks the item completed and sets a storage key.
- Coverage of `internal/workers` goes from 0% to non-zero
  (`go test ./internal/workers/ -cover`).
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go test ./internal/workers/ -count=1` passes with the three new tests
- [ ] `go test ./internal/workers/ -cover -count=1` reports coverage > 0%
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/workers/` prints nothing
- [ ] `git status` shows only `internal/workers/archive_worker_test.go` changed
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- `saveArchiveData` / `processArchiveJob` / `uploadNonce` signatures differ from
  the "Current state" excerpts.
- The `dataArchiver` stub does not satisfy `archivers.Archiver` (interface changed)
  — report the actual interface.
- `processArchiveJob` requires River or Postgres-only features to run under sqlite
  (it should not; it uses the `db`, storage, and the archiver map) — report the
  failure.

## Maintenance notes

- Deferred coverage: `QueueCapture` (transaction + River insert) and
  `ArchiveWorker.Work` (retry accounting, final-attempt failure) need a
  Postgres+River harness. Add these as a DSN-gated integration test (skip unless
  `ARKER_TEST_POSTGRES_DSN` is set — mirror
  `internal/utils/archive_item_logs_postgres_test.go`), which the Plan 001 CI runs
  against its Postgres service. That test should assert: `QueueCapture` creates the
  URL/capture/items and enqueues jobs; a final-attempt failure flips status to
  `failed` and appends a log (post-Plan-004 behavior).
- Reviewer: confirm these tests do not hit the network or launch a browser (they
  must not) so they stay fast and CI-safe.
