# Plan 006: Set HttpOnly, Secure, and SameSite on the session cookie

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

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

The admin session cookie is created with no explicit options, so the
gin-contrib/gorilla defaults apply: **HttpOnly = false, Secure = false, SameSite
unset**. Without HttpOnly, any script running on the app origin (archived content
is served same-origin — a separate finding) can read the session cookie. Without
Secure, the cookie can be sent over plaintext HTTP. Unset SameSite leaves CSRF
defense entirely to browser defaults. Setting these three flags is a one-block
change that removes the most direct cookie-theft and downgrade paths. Secure is
gated to release mode so local HTTP development still works.

## Current state

`cmd/main.go` — session store setup (lines 496–498):
```go
	r.LoadHTMLGlob("templates/*.html")
	store := cookie.NewStore([]byte(sessionSecret))
	r.Use(sessions.Sessions("session", store))
```

- A repo-wide search confirms no cookie options are set anywhere:
  `grep -rn "SameSite\|HttpOnly\|store.Options\|\.Options(" --include=*.go .`
  returns no results in application code today.
- `cmd/main.go` already imports `github.com/gin-contrib/sessions` (line 15) and
  `github.com/gin-contrib/sessions/cookie` (line 16). It does **not** currently
  import `net/http` — you will add it.
- `normalizeGinMode` (cmd/main.go:209) returns `gin.ReleaseMode` /
  `gin.DebugMode` / `gin.TestMode`; the effective mode is set at line 254 with
  `gin.SetMode(normalizeGinMode(cfg.GinMode))`. Use `gin.Mode()` to read it back.

## Commands you will need

| Purpose      | Command                            | Expected   |
|--------------|------------------------------------|------------|
| Build        | `go build ./...`                   | exit 0     |
| Vet          | `go vet ./...`                     | exit 0     |
| Format check | `gofmt -l cmd/main.go`             | no output  |
| Tests (all)  | `go test ./... -count=1`           | no `FAIL`  |

## Scope

**In scope**:
- `cmd/main.go` (add the `net/http` import and a `store.Options(...)` call)

**Out of scope**:
- Any handler, template, or the login flow. Do not add CSRF tokens here (separate,
  unselected finding).
- The session secret handling above line 496 — unchanged.

## Git workflow

- Branch: `advisor/006-session-cookie-flags`
- Commit message: `Harden session cookie: HttpOnly, Secure, SameSite`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add the `net/http` import

In `cmd/main.go`'s import block, add `"net/http"` (keep imports grouped/sorted as
the file already does — it belongs with the stdlib group near `"os"`, `"strings"`,
`"time"`). `gofmt` will confirm ordering.

### Step 2: Set cookie options before registering the middleware

Change lines 497–498 to configure the store first:
```go
	store := cookie.NewStore([]byte(sessionSecret))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   7 * 24 * 60 * 60, // 7 days
		HttpOnly: true,
		Secure:   gin.Mode() == gin.ReleaseMode, // HTTPS-only in production; allows local HTTP dev
		SameSite: http.SameSiteLaxMode,
	})
	r.Use(sessions.Sessions("session", store))
```

**Verify**: `go build ./...` → exit 0; and
`grep -n "HttpOnly: true" cmd/main.go` → returns one line.

## Test plan

- No new automated test is required (this is a wiring/config change and there is
  no existing session-cookie test harness). Verification is build + a manual
  smoke check described below.
- Manual smoke (optional, only if a local run is convenient): start the app in
  release mode behind HTTPS (or in debug mode over HTTP), log in, and inspect the
  `session` cookie in browser devtools — it should show `HttpOnly` and, in
  release, `Secure`, with `SameSite=Lax`.
- Full suite: `go test ./... -count=1` → no `FAIL` (confirms nothing else broke).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "HttpOnly: true" cmd/main.go` returns one line
- [ ] `grep -n "SameSite: http.SameSiteLaxMode" cmd/main.go` returns one line
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l cmd/main.go` prints nothing
- [ ] `git status` shows only `cmd/main.go` modified
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- Lines 496–498 do not match the "Current state" excerpt.
- `sessions.Options` does not have the fields used above (API differs in the
  installed `gin-contrib/sessions` version) — report the actual `Options` struct
  from the dependency and stop.
- Setting `Secure: true` in a test context breaks an existing test that logs in
  over HTTP (unlikely, since gating is on release mode) — report which test.

## Maintenance notes

- `Secure` is gated on `gin.Mode() == gin.ReleaseMode`. Production runs in release
  mode (the default; `cmd/main.go:48`), so cookies are HTTPS-only in prod while
  local `GIN_MODE=debug` development over HTTP keeps working. If a future
  deployment serves the app over plain HTTP in release mode, logins will break —
  that is the correct signal to fix the transport, not to unset Secure.
- `SameSite=Lax` still permits top-level GET navigations to carry the cookie;
  `Strict` would be safer but can surprise users following external links into the
  admin UI. Revisit alongside the CSRF-token finding if that gets picked up.
