# Plan 009: Add database indexes for the FK / status / type columns the hot queries filter on

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/models/models.go cmd/main.go`
> If either file changed since this plan was written, compare the "Current state"
> excerpts against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: perf
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`internal/models/models.go` declares no index tags, and GORM AutoMigrate does not
create indexes on foreign-key columns for Postgres. Yet the hot query paths filter
on exactly those columns:

- `archive_items.capture_id` + `type` — every archive lookup and the git handler
  (`handlers/git.go:40`), the worker (`archive_worker.go:64`).
- `archive_items.status` (+ `created_at`) — the admin dashboard counts
  (`admin.go:76-84`) and, most importantly, `calculateQueuePosition`
  (`display.go:20-23`) runs `WHERE status='pending' AND created_at < ?` on **every
  archive page view**.
- `captures.archived_url_id` — past-archives lookup (`api.go:47`) and the
  dashboard join.

At the ~100k-capture scale the code itself anticipates (`admin.go` paginates in
1000s), these are sequential scans. Adding covering indexes turns them into index
lookups. The change is additive and low-risk.

## Current state

`internal/models/models.go` — no index tags today. Relevant models:
```go
type Capture struct {
	gorm.Model
	ArchivedURLID uint
	ArchivedURL   ArchivedURL `gorm:"foreignKey:ArchivedURLID"`
	Timestamp     time.Time
	ShortID       string        `gorm:"unique"`
	APIKeyID      *uint         `gorm:"nullable"`
	APIKey        *APIKey       `gorm:"foreignKey:APIKeyID"`
	ArchiveItems  []ArchiveItem `gorm:"foreignKey:CaptureID"`
}

type ArchiveItem struct {
	gorm.Model
	CaptureID  uint
	Type       string // mhtml, screenshot, git, youtube
	Status     string // pending, processing, completed, failed
	StorageKey string
	Extension  string
	FileSize   int64
	Logs       string `gorm:"type:text"`
	RetryCount int
}
```
`CreatedAt` is part of the embedded `gorm.Model`, so the `(status, created_at)`
composite cannot be expressed as a struct tag — it is added via raw SQL after
AutoMigrate (portable across Postgres and the sqlite used in tests).

`cmd/main.go` — AutoMigrate call (lines 284–287):
```go
	if err := db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}, &models.Config{}); err != nil {
		slog.Error("AutoMigrate failed with detailed error", "error", err, "error_type", fmt.Sprintf("%T", err), "error_string", err.Error())
		slog.Info("Continuing startup despite AutoMigrate error")
	}
```

## Commands you will need

| Purpose      | Command                                        | Expected   |
|--------------|------------------------------------------------|------------|
| Build        | `go build ./...`                               | exit 0     |
| Vet          | `go vet ./...`                                 | exit 0     |
| Format check | `gofmt -l internal/models/models.go cmd/main.go` | no output |
| Tests (all)  | `go test ./... -count=1`                       | no `FAIL`  |

## Scope

**In scope**:
- `internal/models/models.go` (add index tags)
- `cmd/main.go` (add one composite-index `Exec` after AutoMigrate)

**Out of scope**:
- Query code in handlers/workers — do not rewrite queries.
- The River `river_job` table indexes (managed by River migrations).

## Git workflow

- Branch: `advisor/009-db-indexes`
- Commit message: `Add indexes for archive_items and captures hot-path columns`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add index tags for the FK and (capture_id, type) columns

In `internal/models/models.go`:

- On `Capture.ArchivedURLID`, add a single-column index:
  ```go
  	ArchivedURLID uint `gorm:"index"`
  ```
- On `ArchiveItem`, make `CaptureID` + `Type` a composite index by tagging both
  with the same index name:
  ```go
  	CaptureID uint   `gorm:"index:idx_archive_items_capture_type,priority:1"`
  	Type      string `gorm:"index:idx_archive_items_capture_type,priority:2"` // mhtml, screenshot, git, youtube
  ```

Leave all other fields unchanged.

**Verify**: `go build ./...` → exit 0; `grep -n "idx_archive_items_capture_type" internal/models/models.go`
returns two lines.

### Step 2: Add the (status, created_at) composite index at startup

In `cmd/main.go`, immediately after the AutoMigrate block (after line 287), add:
```go
	// Composite index for the queue-position and status-count queries that run on
	// every archive page view / dashboard load. CreatedAt is embedded in
	// gorm.Model, so this is expressed as raw SQL rather than a struct tag.
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_archive_items_status_created_at ON archive_items (status, created_at)").Error; err != nil {
		slog.Error("Failed to create archive_items status/created_at index", "error", err)
	}
```

`db` and `slog` are already in scope here.

**Verify**: `go build ./...` → exit 0; `grep -n "idx_archive_items_status_created_at" cmd/main.go`
returns one line.

## Test plan

- No new Go test is strictly required (indexes are a performance concern, not a
  behavioral one). The existing suite must still pass, which confirms the tags and
  the raw `CREATE INDEX` do not break AutoMigrate on sqlite (the test dialect).
- Full suite: `go test ./... -count=1` → no `FAIL`.
- **Production verification** (manual, by the operator, not the executor): after
  deploy, connect to Postgres and run `\d archive_items` and `\d captures`;
  confirm `idx_archive_items_capture_type`, `idx_archive_items_status_created_at`,
  and an index on `captures.archived_url_id` exist.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "idx_archive_items_capture_type" internal/models/models.go` returns two lines
- [ ] `grep -n "gorm:\"index\"" internal/models/models.go` includes `ArchivedURLID`
- [ ] `grep -n "idx_archive_items_status_created_at" cmd/main.go` returns one line
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/models/models.go cmd/main.go` prints nothing
- [ ] `git status` shows only `models.go` and `cmd/main.go` modified
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- The `Capture`/`ArchiveItem` structs do not match the "Current state" excerpt.
- AutoMigrate errors on sqlite after adding the tags (run `go test ./... -count=1`
  — the handler/worker tests AutoMigrate these models on sqlite).
- The raw `CREATE INDEX` fails on sqlite in tests (it should not — `IF NOT EXISTS`
  and `(status, created_at)` are valid sqlite syntax); if it does, report the error.

## Maintenance notes

- On the next production deploy, AutoMigrate will build `idx_archive_items_capture_type`
  and the `ArchivedURLID` index with a brief table lock (`CREATE INDEX`, not
  `CONCURRENTLY`). At ~100k rows this is seconds. For zero-downtime, an operator
  can pre-create the indexes with `CREATE INDEX CONCURRENTLY` before deploying;
  AutoMigrate then no-ops on the existing indexes.
- If a future query filters `archive_items` by `status` alone with high
  selectivity, the `(status, created_at)` composite already serves it (status is
  the leading column).
- Reviewer: confirm no duplicate/redundant index is introduced (GORM skips
  existing indexes by name).
