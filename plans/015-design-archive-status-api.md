# Plan 015: Design spike — a public archive-status API endpoint

> **Executor instructions**: This is a **design/spike** plan. Its primary
> deliverable is a written proposal, not a shipped feature. Do the investigation,
> write the proposal document, and STOP for maintainer review before building the
> production endpoint. An optional prototype step is clearly marked. Follow the
> steps, honor the STOP conditions, and update this plan's row in
> `plans/README.md` when the proposal is written.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/handlers/api.go cmd/main.go internal/models/models.go`
> If any changed since this plan was written, re-read them before writing the
> proposal.

## Status

- **Priority**: P3
- **Effort**: M (spike: mostly design + a thin prototype)
- **Risk**: LOW
- **Depends on**: none (a prototype is easier after Plan 012's route-group pattern,
  but not required)
- **Category**: direction
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`POST /api/v1/archive` returns a short-URL immediately, but archiving is
asynchronous (yt-dlp/Playwright can take minutes, or fail). The only other API
route is `GET /api/v1/past-archives`. There is **no** API way to learn whether a
given capture's archive types have completed, are still processing, or failed — an
API consumer must scrape the HTML display page or poll the unauthenticated
`/logs` endpoint. The status data already exists (`ArchiveItem.Status/Type/
Extension/FileSize`) and the web UI already polls it; exposing it as a first-class
API is the single highest-value, lowest-cost capability for anyone building on
Arker (e.g. other Hack Club services using it as an archiving backend). This spike
defines the endpoint contract and open questions so the maintainer can approve a
shape before it is built.

## Current state (evidence to ground the design)

- `internal/handlers/api.go` — the existing API handlers and the shared
  `getPastArchives` pattern to mirror (`ApiArchive` at line 71, `ApiPastArchives`
  at line 61). Note the response-struct + `c.JSON` style:
  ```go
  type PastArchiveResponse struct {
  	ShortID   string    `json:"short_id"`
  	Timestamp time.Time `json:"timestamp"`
  }
  ```
- `internal/models/models.go` — `Capture` (has `ShortID`, `Timestamp`,
  `ArchivedURLID`, `ArchiveItems []ArchiveItem`) and `ArchiveItem` (`Type`,
  `Status` = pending/processing/completed/failed, `Extension`, `FileSize`).
- `cmd/main.go:530-531` — API routes are registered with the `RequireAPIKey(db)`
  middleware:
  ```go
  	r.POST("/api/v1/archive", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiArchive(c, db, riverClient) })
  	r.GET("/api/v1/past-archives", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiPastArchives(c, db) })
  ```
- `internal/handlers/display.go` — the display handler already loads a capture with
  `Preload("ArchiveItems")` and computes per-type view state; reuse its query
  shape, not its HTML rendering.
- The itch/video web UI polls status via `GET /logs/:shortid/:type` today
  (`templates/display_type.html:360`), which returns `{logs, status, retry_count}`.

## Commands you will need (for the optional prototype)

| Purpose      | Command                                        | Expected   |
|--------------|------------------------------------------------|------------|
| Build        | `go build ./...`                               | exit 0     |
| Vet          | `go vet ./...`                                 | exit 0     |
| Tests (all)  | `go test ./... -count=1`                       | no `FAIL`  |

## Scope

**In scope**:
- `docs/proposals/archive-status-api.md` (create — the proposal; make the `docs/`
  and `docs/proposals/` directories if absent)
- **Optional, only if the proposal's own design is unambiguous and the maintainer
  is unavailable to review first**: a prototype `GET /api/v1/archive/:shortid`
  handler in `internal/handlers/api.go` + route in `cmd/main.go` + a handler test.

**Out of scope**:
- Webhooks, scheduled re-archiving, deletion — separate direction findings.
- Changing the existing `/api/v1/archive` or `/past-archives` response shapes.
- Auth mechanism changes — reuse `RequireAPIKey`.

## Git workflow

- Branch: `advisor/015-archive-status-api-spike`
- Commit message: `Add archive-status API design proposal`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Write the proposal

Create `docs/proposals/archive-status-api.md` covering:

1. **Endpoint**: propose `GET /api/v1/archive/:shortid`, behind `RequireAPIKey`.
   State the alternative (`GET /api/v1/status?url=...` mirroring `past-archives`)
   and why shortID is preferred (a capture is the unit of async work).
2. **Response schema**: a concrete JSON example, e.g.
   ```json
   {
     "short_id": "p9OGi",
     "url": "https://example.com",
     "timestamp": "2026-07-07T12:00:00Z",
     "items": [
       {"type": "mhtml", "status": "completed", "extension": ".mhtml", "file_size": 12345},
       {"type": "screenshot", "status": "processing"},
       {"type": "git", "status": "failed"}
     ],
     "done": false
   }
   ```
   Map each field to its `models.ArchiveItem` source. Define `done` (all items in a
   terminal state: completed or failed).
3. **Type naming**: reconcile the internal `mhtml` type with the user-facing `web`
   label (see `urlTypeToInternalType`/`internalTypeToURLType` in `display.go`).
   Decide which the API exposes and document it.
4. **Not-found & auth behavior**: 404 for unknown shortID; 401 without a valid key
   (consistent with existing API handlers).
5. **Reuse**: reference `getPastArchives` (api.go) and the display query as the
   implementation pattern; note `FileSize`/`Extension` are only meaningful once
   `status == "completed"`.
6. **Open questions** (list, don't answer): Should logs be included or linked?
   Should it accept `?url=` to return the latest capture's status? Rate limiting?
   Should it supersede the web UI's `/logs` polling (which currently leaks — see
   the log-redaction plan)? Pagination if a capture ever has many item types?

**Verify**: `docs/proposals/archive-status-api.md` exists and contains a JSON
response example and an "Open questions" section
(`grep -c "Open questions" docs/proposals/archive-status-api.md` ≥ 1).

### Step 2 (OPTIONAL prototype — only if instructed or maintainer unavailable)

If and only if you are explicitly told to prototype, implement the minimal read
endpoint:
- Add `ApiArchiveStatus(c *gin.Context, db *gorm.DB)` to `internal/handlers/api.go`,
  modeled on `getPastArchives`: look up the `Capture` by `short_id` with
  `Preload("ArchiveItems")`, load the `ArchivedURL` for the URL, and return the
  schema from the proposal. 404 on not found.
- Register the route in `cmd/main.go` next to the other API routes:
  ```go
  	r.GET("/api/v1/archive/:shortid", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiArchiveStatus(c, db) })
  ```
- Add a handler test in `internal/handlers/` modeled on the sqlite harness in
  `logs_test.go`: seed a capture with items in mixed statuses, call the handler,
  assert the JSON includes each item's status and the correct `done` value. (The
  test can call the handler with a session-less context if you factor the core
  logic out of the `RequireAPIKey` middleware, or seed a valid API key.)

**Verify**: `go build ./... && go test ./... -count=1` → build exit 0, no `FAIL`.

## Test plan

- Spike deliverable (Step 1) needs no automated test — it is a document.
- If Step 2 is done: one handler test seeding mixed-status items and asserting the
  response schema and `done` flag. Model after `internal/handlers/logs_test.go`.

## Done criteria

- [ ] `docs/proposals/archive-status-api.md` exists with: proposed endpoint, a
      concrete JSON response example, field→model mapping, and an "Open questions"
      section
- [ ] If Step 2 was performed: `go build ./...` exits 0, the new route is
      registered behind `RequireAPIKey`, and its test passes; otherwise Step 2 is
      untouched
- [ ] `plans/README.md` status row updated (note whether the prototype was built)

## STOP conditions

Stop and report back (do not build the production endpoint) if:

- Any open question in the proposal materially changes the endpoint's shape (e.g.
  the maintainer wants `?url=` semantics or logs included) — those are decisions
  for the maintainer.
- The existing API's auth or response conventions differ from the "Current state"
  excerpts.
- Building the prototype would require changing `RequireAPIKey` or an existing
  response shape.

## Maintenance notes

- This endpoint pairs naturally with the completion-webhook direction finding
  (poll now, push later); note that relationship in the proposal.
- If built, it is also the clean replacement for the web UI's reliance on the
  public `/logs` endpoint for status polling — coordinate with the log-redaction
  plan.
- Reviewer: the value here is the contract. Scrutinize the schema and the
  `done`/type-naming decisions more than the implementation.
