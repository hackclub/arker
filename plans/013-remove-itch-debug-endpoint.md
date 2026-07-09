# Plan 013: Remove the public itch debug endpoint and its fabricated-metadata handler

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/handlers/itch_debug.go cmd/main.go`
> If either file changed since this plan was written, compare the "Current state"
> excerpts against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: tech-debt
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`internal/handlers/itch_debug.go` defines `ServeItchDebug`, a **public,
unauthenticated** endpoint (`GET /itch/:shortid/debug`) that returns hardcoded,
**fabricated** game metadata — title "Find the Light", author "suri-xoxo",
`game_id: 1234567` — for any archived itch shortID. Git history labels it a
"temporary debug metadata endpoint for testing UI". Shipping it in production is a
correctness/trust hazard (it can shadow real itch responses during debugging and
serves fake data under a real short-id namespace) and it echoes the raw DB error
string on lookup failure. It has no callers other than its route registration.
Removing it tightens the public surface with zero feature loss.

## Current state

`internal/handlers/itch_debug.go` — the whole file is the debug handler
(58 lines). It returns fabricated fields:
```go
func ServeItchDebug(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	// ...
	c.JSON(http.StatusOK, gin.H{
		"game_id": 1234567,
		"title":   "Find the Light",
		"url":     archivedURL.Original,
		"author":  "suri-xoxo",
		// ...
	})
}
```
and on lookup failure returns `"query_error": err.Error()` (raw DB error).

`cmd/main.go` — the route registration (line 540):
```go
	r.GET("/itch/:shortid/debug", func(c *gin.Context) { handlers.ServeItchDebug(c, storageInstance, db) })
```

Confirm there are no other references before deleting:
`grep -rn "ServeItchDebug" --include=*.go .` should return exactly two matches —
the definition in `itch_debug.go` and the route in `cmd/main.go`.

## Commands you will need

| Purpose      | Command                                        | Expected   |
|--------------|------------------------------------------------|------------|
| Build        | `go build ./...`                               | exit 0     |
| Vet          | `go vet ./...`                                 | exit 0     |
| Reference    | `grep -rn "ServeItchDebug" --include=*.go .`   | no matches after deletion |
| Format check | `gofmt -l cmd/main.go`                         | no output  |
| Tests (all)  | `go test ./... -count=1`                       | no `FAIL`  |

## Scope

**In scope**:
- `internal/handlers/itch_debug.go` (delete the file)
- `cmd/main.go` (remove the one route registration line)

**Out of scope**:
- The real itch serving handlers (`internal/handlers/itch_serve.go`) — keep them.
  Note: `itch_serve.go` also emits `X-Debug-*` headers and a 200MB refusal path;
  those are separate findings, not part of this plan.
- `internal/handlers/itch_serve.go`'s `ServeItchHealth` and the other `/itch/*`
  routes — keep them.

## Git workflow

- Branch: `advisor/013-remove-itch-debug`
- Commit message: `Remove temporary itch debug endpoint`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Confirm the endpoint is unused elsewhere

Run `grep -rn "ServeItchDebug" --include=*.go .`. Expect exactly two matches:
the definition and the route. If there are more (e.g. a test), STOP and report.

### Step 2: Delete the route registration

In `cmd/main.go`, delete line 540:
```go
	r.GET("/itch/:shortid/debug", func(c *gin.Context) { handlers.ServeItchDebug(c, storageInstance, db) })
```
Leave the surrounding itch routes (`/itch/health`, `/itch/:shortid/file/*filepath`,
`/itch/:shortid/list`) intact.

### Step 3: Delete the handler file

Delete `internal/handlers/itch_debug.go` entirely (e.g. `rm internal/handlers/itch_debug.go`).

**Verify**: `go build ./...` → exit 0 (no unused-import or missing-symbol errors);
`grep -rn "ServeItchDebug" --include=*.go .` → no matches.

## Test plan

- No new test. Deletion is verified by a clean build and the reference grep
  returning nothing.
- Full suite: `go test ./... -count=1` → no `FAIL` (confirms no test depended on
  the debug endpoint).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `internal/handlers/itch_debug.go` no longer exists
- [ ] `grep -rn "ServeItchDebug" --include=*.go .` returns no matches
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l cmd/main.go` prints nothing
- [ ] `git status` shows `itch_debug.go` deleted and `cmd/main.go` modified only
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- `grep -rn "ServeItchDebug"` finds a reference outside `itch_debug.go` and
  `cmd/main.go` (e.g. a test or template) — do not delete blindly; report it.
- Removing the route leaves an unused import in `cmd/main.go` that you cannot
  resolve by deleting only the now-dead import (there should be none — `handlers`
  and `storage` are still used by other routes).
- The build fails after deletion for any reason not explained above.

## Maintenance notes

- If a real itch metadata endpoint is ever wanted, build it from the actual
  archive contents (the itch serving path already reads the ZIP), not hardcoded
  values, and gate any debug variant behind `GIN_MODE=debug` or a build tag.
- Related follow-ups (separate findings, not this plan): the `X-Debug-*` response
  headers in `itch_serve.go` and the raw-error exposure there.
