# Plan 008: Require admin auth on the metrics/status endpoints

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- cmd/main.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none (coordinate ordering with Plan 012, which also edits the
  `cmd/main.go` route block — do one at a time and re-run the drift check)
- **Category**: security
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

Three operational endpoints are registered with no authentication and expose
infrastructure internals to anyone:

- `/metrics/browser` and `/status/browser` — Chrome process counts, goroutine
  counts, and browser-leak detection state (`monitoring.go:26-61`).
- `/status/db-storage` — host disk total/used/available bytes and Postgres
  database/table/TOAST sizes (`monitoring.go:65-114`).

This is free reconnaissance (disk pressure, DB growth, process internals) useful
for planning resource-exhaustion or timing attacks. It is also inconsistent: the
River queue UI at `/queue` **is** gated by `RequireLogin`. `/health` should stay
public (it is a minimal liveness probe with no sensitive data). This plan gates
the `/metrics/*` and `/status/*` routes behind the existing session login.

## Current state

`cmd/main.go` — route registration (lines 501–504):
```go
	r.GET("/health", handlers.HealthCheckHandler(db))
	r.GET("/metrics/browser", handlers.BrowserMetricsHandler())
	r.GET("/status/browser", handlers.BrowserStatusHandler())
	r.GET("/status/db-storage", handlers.DBStorageStatusHandler(db))
```

The existing auth helper (`internal/handlers/auth.go:42`) is a per-request guard:
```go
func RequireLogin(c *gin.Context) bool {
	session := sessions.Default(c)
	if session.Get("user_id") == nil {
		c.Redirect(http.StatusFound, "/login")
		return false
	}
	return true
}
```
It is already used inline for other protected routes, e.g. the `/queue` handlers
(`cmd/main.go:517-528`):
```go
	r.GET("/queue", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		riverUIServer.ServeHTTP(c.Writer, c.Request)
	})
```

## Commands you will need

| Purpose      | Command                    | Expected   |
|--------------|----------------------------|------------|
| Build        | `go build ./...`           | exit 0     |
| Vet          | `go vet ./...`             | exit 0     |
| Format check | `gofmt -l cmd/main.go`     | no output  |
| Tests (all)  | `go test ./... -count=1`   | no `FAIL`  |

## Scope

**In scope**:
- `cmd/main.go` (wrap three route handlers with a login check)

**Out of scope**:
- `/health` — leave it public.
- `internal/handlers/monitoring.go` — no handler-body changes; only the route
  registration changes.
- The broader "move admin routes to a middleware group" refactor — that is Plan
  012. This plan uses the same inline `RequireLogin` pattern already in the file
  to stay minimal and independently reviewable.

## Git workflow

- Branch: `advisor/008-auth-metrics`
- Commit message: `Require login for metrics/status endpoints`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Gate the three endpoints

Replace lines 502–504 with login-guarded wrappers, matching the `/queue` pattern
already in the file:
```go
	r.GET("/metrics/browser", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		handlers.BrowserMetricsHandler()(c)
	})
	r.GET("/status/browser", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		handlers.BrowserStatusHandler()(c)
	})
	r.GET("/status/db-storage", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		handlers.DBStorageStatusHandler(db)(c)
	})
```

Note the handlers are factories returning `gin.HandlerFunc`; call the factory then
invoke the result with `(c)` as shown. Leave the `/health` line (501) unchanged.

**Verify**: `go build ./...` → exit 0; and
`grep -c "RequireLogin" cmd/main.go` → increases by 3 relative to before.

## Test plan

- No new automated test (there is no existing HTTP-route test harness in `cmd/`).
  Verification is build + the grep checks. Optionally, if convenient, run the app
  and confirm `curl -i localhost:8080/status/browser` returns a 302 redirect to
  `/login` while unauthenticated, and `curl -i localhost:8080/health` still
  returns 200.
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `/metrics/browser`, `/status/browser`, `/status/db-storage` each call
      `handlers.RequireLogin` (grep: `grep -n "RequireLogin" cmd/main.go` shows the
      three new sites plus the pre-existing ones)
- [ ] `/health` registration is unchanged and still public
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l cmd/main.go` prints nothing
- [ ] `git status` shows only `cmd/main.go` modified
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- Lines 501–504 do not match the "Current state" excerpt.
- The monitoring handlers are not factories returning `gin.HandlerFunc` (so the
  `Handler()(c)` call shape does not compile) — report the actual signatures from
  `internal/handlers/monitoring.go`.
- Plan 012 has already restructured these routes into a group — if so, this plan
  may be redundant; report and stop rather than double-gating.

## Maintenance notes

- If Plan 012 (admin auth middleware group) lands later, fold these three routes
  into the authenticated group and drop the inline checks for consistency.
- `/health` stays public for load-balancer/Coolify liveness checks; keep it free
  of sensitive data.
