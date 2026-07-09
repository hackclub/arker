# Plan 012: Enforce admin auth with route-group middleware instead of per-handler checks

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat dcd7526..HEAD -- cmd/main.go internal/handlers/admin.go internal/handlers/api_keys.go internal/handlers/auth.go`
> If any of these changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch, treat
> it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: MED
- **Depends on**: 001. **Coordinate with Plan 008** (also edits the `cmd/main.go`
  route block): land 008 first if doing both, then re-run this plan's drift check.
- **Category**: tech-debt (security-adjacent)
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

Every `/admin/*` handler enforces authentication by calling
`if !RequireLogin(c) { return }` as its first statement — nine handlers, each
opting in individually. A newly-added `/admin/*` handler that forgets that line is
silently public. Meanwhile API-key auth is already done correctly as middleware
(`RequireAPIKey`). This plan moves the nine `/admin/*` routes under a Gin route
group guarded by a single `RequireLogin` middleware, making auth the default for
that prefix, and removes the now-redundant per-handler checks. Behavior is
unchanged for every route; the difference is that forgetting the guard is no
longer possible for anything under `/admin`.

## Current state

`internal/handlers/auth.go` — the existing per-request guard (lines 42–49):
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

`cmd/main.go` — the nine `/admin/*` route registrations (lines 507–515):
```go
	r.GET("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	r.POST("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	r.POST("/admin/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	r.DELETE("/admin/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
	r.POST("/admin/retry-failed", func(c *gin.Context) { handlers.RetryAllFailedJobs(c, db, riverClient) })
	r.POST("/admin/backfill-videos", func(c *gin.Context) { handlers.BackfillMissingVideoItems(c, db, riverClient) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db, riverClient) })
	r.POST("/admin/archive", func(c *gin.Context) { handlers.AdminArchive(c, db, riverClient) })
	r.GET("/admin/item/:id/log", func(c *gin.Context) { handlers.GetItemLog(c, db) })
```

The nine handlers each begin with the **identical** 3-line block. Confirmed
locations:
- `internal/handlers/api_keys.go`: `ApiKeysGet` (line 15), `ApiKeysCreate` (26),
  `ApiKeysToggle` (98), `ApiKeysDelete` (129).
- `internal/handlers/admin.go`: `RequestCapture` (106), `GetItemLog` (130),
  `RetryAllFailedJobs` (150), `BackfillMissingVideoItems` (217), and `AdminArchive`
  (the handler routed at `POST /admin/archive`).

The block in each is exactly:
```go
	if !RequireLogin(c) {
		return
	}
```

**Do NOT touch** `AdminGet` in `admin.go` (line 20): it serves `GET /` (the
dashboard), which is **not** under the `/admin` prefix, so it keeps its inline
check. Likewise the two `/queue` closures in `cmd/main.go:517-528` keep their
inline checks (not under `/admin`).

## Commands you will need

| Purpose      | Command                                                              | Expected   |
|--------------|---------------------------------------------------------------------|------------|
| Build        | `go build ./...`                                                    | exit 0     |
| Vet          | `go vet ./...`                                                      | exit 0     |
| Middleware test | `go test ./internal/handlers/ -run TestRequireLoginMiddleware -count=1` | `ok`  |
| Format check | `gofmt -l cmd/main.go internal/handlers/auth.go internal/handlers/admin.go internal/handlers/api_keys.go` | no output |
| Tests (all)  | `go test ./... -count=1`                                            | no `FAIL`  |

## Scope

**In scope**:
- `internal/handlers/auth.go` (add `RequireLoginMiddleware`)
- `cmd/main.go` (convert nine `/admin/*` routes to a guarded group)
- `internal/handlers/admin.go` (remove inline checks from 5 handlers; keep AdminGet)
- `internal/handlers/api_keys.go` (remove inline checks from 4 handlers)
- `internal/handlers/auth_middleware_test.go` (create — middleware unit test)

**Out of scope**:
- `AdminGet` (GET /) and the `/queue` closures — keep their inline checks.
- `RequireAPIKey` and the `/api/v1/*` routes — already correct.
- The `/metrics`, `/status` routes — that is Plan 008.
- Any handler body logic other than deleting the 3-line auth block.

## Git workflow

- Branch: `advisor/012-admin-auth-middleware`
- Commit message: `Guard /admin routes with login middleware group`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add the middleware

In `internal/handlers/auth.go`, add below `RequireLogin`:
```go
// RequireLoginMiddleware is the route-group form of RequireLogin. Attach it to a
// Gin group so every route under it requires an authenticated session; on failure
// RequireLogin issues the redirect and this aborts the chain.
func RequireLoginMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequireLogin(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}
```

**Verify**: `go build ./...` → exit 0.

### Step 2: Convert the /admin routes to a guarded group

In `cmd/main.go`, replace the nine lines (507–515) with a group. Note the paths
drop the `/admin` prefix because the group supplies it:
```go
	admin := r.Group("/admin", handlers.RequireLoginMiddleware())
	admin.GET("/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	admin.POST("/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	admin.POST("/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	admin.DELETE("/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
	admin.POST("/retry-failed", func(c *gin.Context) { handlers.RetryAllFailedJobs(c, db, riverClient) })
	admin.POST("/backfill-videos", func(c *gin.Context) { handlers.BackfillMissingVideoItems(c, db, riverClient) })
	admin.POST("/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db, riverClient) })
	admin.POST("/archive", func(c *gin.Context) { handlers.AdminArchive(c, db, riverClient) })
	admin.GET("/item/:id/log", func(c *gin.Context) { handlers.GetItemLog(c, db) })
```

Double-check each path maps to the same full URL as before (group prefix `/admin`
+ the sub-path equals the original). Leave all other route registrations (health,
metrics, login, queue, api, archive, itch, git, catch-alls) untouched.

**Verify**: `go build ./...` → exit 0.

### Step 3: Remove the redundant inline checks from the nine handlers

Delete the exact 3-line block
```go
	if !RequireLogin(c) {
		return
	}
```
from the top of each of: `ApiKeysGet`, `ApiKeysCreate`, `ApiKeysToggle`,
`ApiKeysDelete` (in `api_keys.go`); and `RequestCapture`, `GetItemLog`,
`RetryAllFailedJobs`, `BackfillMissingVideoItems`, `AdminArchive` (in `admin.go`).

Do NOT remove it from `AdminGet`.

**Verify**: `grep -rn "if !RequireLogin(c)" internal/handlers/` → returns exactly
ONE match (inside `AdminGet` in `admin.go`). And
`grep -n "RequireLogin(c)" internal/handlers/auth.go` shows the call inside
`RequireLoginMiddleware`.

### Step 4: Add a middleware unit test

Create `internal/handlers/auth_middleware_test.go`. It builds a minimal Gin engine
with the session middleware and a group guarded by `RequireLoginMiddleware`, then
asserts: unauthenticated → 302 redirect to `/login`; authenticated → 200.

```go
package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestRequireLoginMiddlewareBlocksAnonymous(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	store := cookie.NewStore([]byte("test-secret"))
	r.Use(sessions.Sessions("session", store))

	// A login route so the redirect target exists.
	r.GET("/login", func(c *gin.Context) { c.String(http.StatusOK, "login") })

	grp := r.Group("/admin", RequireLoginMiddleware())
	grp.GET("/secret", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Unauthenticated: expect a redirect to /login (302), not 200.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/secret", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("anonymous request: got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Fatalf("redirect location = %q, want /login", loc)
	}
}
```

**Verify**:
`go test ./internal/handlers/ -run TestRequireLoginMiddleware -count=1` → `ok`.

## Test plan

- New middleware test proves an unauthenticated request to a guarded group route
  is redirected (302 → `/login`) rather than reaching the handler.
- The full suite must still pass, confirming no route was accidentally moved or
  dropped: `go test ./... -count=1` → no `FAIL`.
- Manual spot check (optional): run the app, and while logged out
  `curl -s -o /dev/null -w "%{http_code}" localhost:8080/admin/api-keys` → `302`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -rn "if !RequireLogin(c)" internal/handlers/` returns exactly ONE match
      (in `AdminGet`)
- [ ] `cmd/main.go` registers the nine admin routes via `r.Group("/admin", handlers.RequireLoginMiddleware())`
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/handlers/ -run TestRequireLoginMiddleware -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l` on the four touched files prints nothing
- [ ] `git status` shows only the in-scope files changed
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- Any handler's inline auth block is NOT the exact 3-line form shown (e.g. it has
  extra logic) — report which and stop rather than guessing.
- `AdminArchive` is not found in `admin.go`, or its route is not `POST /admin/archive`
  — report the actual location/route.
- After Step 2, the full URL for any admin route would differ from the original
  (group prefix miscomputed) — recheck the mapping and report.
- Plan 008 has already restructured metrics/status routes and the diff conflicts —
  reconcile or report.

## Maintenance notes

- New `/admin/*` routes added to the `admin` group are now authenticated by
  default; contributors should register admin endpoints on `admin`, not `r`.
- If Plan 008's metrics/status routes are later folded into an authenticated group,
  reuse `RequireLoginMiddleware` for consistency.
- Reviewer: verify every one of the nine routes still resolves to the same path and
  method, and that `GET /` (AdminGet) and `/queue` retain their inline checks.
