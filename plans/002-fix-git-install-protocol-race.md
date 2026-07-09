# Plan 002: Register go-git HTTP protocols once, not per job (fix concurrent-map crash)

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/archivers/git.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: 001 (so the new test actually runs under `make test`/CI)
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`GitArchiver.Archive` calls `client.InstallProtocol("https", …)` and
`client.InstallProtocol("http", …)` on **every** invocation. In go-git v5.8.1
`InstallProtocol` writes to a package-global `map` (`Protocols`) with no mutex,
and clones read that same map. River runs archive jobs concurrently (each job in
its own goroutine). Two git jobs running at once — or one registering while
another clones — produce a concurrent map write, which the Go runtime turns into
`fatal error: concurrent map writes`. That is **not** a recoverable panic: it
crashes the entire server process, killing all in-flight archives and the HTTP
server. Registering the protocol client exactly once removes the concurrent
write while preserving identical behavior (the client is a process-global anyway).

## Current state

- `internal/archivers/git.go` — the archiver. Relevant excerpts:

  Existing one-time HTTP client (already uses `sync.Once` — follow this pattern),
  lines 24–45:
  ```go
  var (
  	gitHTTPClientOnce sync.Once
  	gitHTTPClient     *http.Client
  )

  func getGitHTTPClient() *http.Client {
  	gitHTTPClientOnce.Do(func() {
  		transport := &http.Transport{ /* ... */ }
  		gitHTTPClient = &http.Client{ Transport: transport, Timeout: 5 * time.Minute }
  	})
  	return gitHTTPClient
  }
  ```

  The per-call registration inside `Archive` (lines 57–62) — this is the bug:
  ```go
  	// Configure HTTP client with pooled connections
  	httpClient := getGitHTTPClient()

  	// Install pooled HTTP client for git operations
  	client.InstallProtocol("https", githttp.NewClient(httpClient))
  	client.InstallProtocol("http", githttp.NewClient(httpClient))
  ```

- The unsynchronized global map in the dependency (for context; do not edit
  vendored code): `go-git/v5/plumbing/transport/client/client.go` declares
  `var Protocols = map[string]transport.Transport{…}` and `InstallProtocol`
  does `Protocols[scheme] = c` with no lock.
- `sync` is already imported in `git.go` (line 17).

## Commands you will need

| Purpose       | Command                                                   | Expected                     |
|---------------|----------------------------------------------------------|------------------------------|
| Build         | `go build ./...`                                          | exit 0                       |
| Vet           | `go vet ./...`                                            | exit 0                       |
| Race test     | `go test ./internal/archivers/ -run TestInstallGitProtocols -race -count=1` | `ok`, no `DATA RACE`/`fatal` |
| Format check  | `gofmt -l internal/archivers/git.go`                     | no output                    |
| Tests (all)   | `go test ./... -count=1`                                  | no `FAIL`                    |

## Scope

**In scope**:
- `internal/archivers/git.go` (edit)
- `internal/archivers/git_race_test.go` (create)

**Out of scope**:
- Any file under the go-git dependency / module cache — never edit vendored deps.
- The tar-streaming goroutine and `unpackGit` — separate concerns (the latter is
  Plan 010).
- `getGitHTTPClient` — leave it exactly as is; reuse it.

## Git workflow

- Branch: `advisor/002-git-install-protocol-once`
- Commit message style: short imperative, e.g.
  `Register go-git protocols once to fix concurrent-map crash`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add a one-time protocol-registration function

In `internal/archivers/git.go`, add a package-level `sync.Once` and a helper that
installs both protocols exactly once. Place it next to `getGitHTTPClient` (after
line 45):

```go
var installGitProtocolsOnce sync.Once

// installGitProtocols registers the pooled HTTP client for git http(s)
// operations exactly once. go-git's client.InstallProtocol writes a global,
// unsynchronized map, so calling it per-job races concurrent git jobs into a
// "concurrent map writes" crash. Registering once is equivalent (the client is
// process-global) and race-free.
func installGitProtocols() {
	installGitProtocolsOnce.Do(func() {
		httpClient := getGitHTTPClient()
		client.InstallProtocol("https", githttp.NewClient(httpClient))
		client.InstallProtocol("http", githttp.NewClient(httpClient))
	})
}
```

### Step 2: Call it from `Archive` instead of registering inline

Replace the per-call block at lines 57–62 with a single call:

```go
	// Register the pooled HTTP client for git operations exactly once.
	installGitProtocols()
```

After this edit, `Archive` no longer calls `client.InstallProtocol` directly and
no longer needs the local `httpClient` variable there.

**Verify**: `go build ./...` → exit 0; and
`grep -n "client.InstallProtocol" internal/archivers/git.go` → the only matches
are inside `installGitProtocols` (the `Once` body), none inside `Archive`.

### Step 3: Add a race regression test

Create `internal/archivers/git_race_test.go`:

```go
package archivers

import (
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

// TestInstallGitProtocols verifies concurrent registration does not race the
// go-git global protocol map (regression for the per-job InstallProtocol crash).
func TestInstallGitProtocols(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			installGitProtocols()
		}()
	}
	wg.Wait()

	if client.Protocols["https"] == nil || client.Protocols["http"] == nil {
		t.Fatal("expected http and https protocols to be registered")
	}
}
```

**Verify**:
`go test ./internal/archivers/ -run TestInstallGitProtocols -race -count=1` →
`ok`, with no `WARNING: DATA RACE` and no `fatal error: concurrent map writes`.

## Test plan

- New test `TestInstallGitProtocols` in `internal/archivers/git_race_test.go`:
  spawns 50 goroutines calling `installGitProtocols()` concurrently and asserts
  both protocols end up registered. Run under `-race` — that is the point.
- Structural pattern: standard Go table/parallel test; no repo-specific harness
  needed (no DB, no storage).
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `grep -n "client.InstallProtocol" internal/archivers/git.go` shows the two
      calls only inside `installGitProtocols`, none in `Archive`
- [ ] `go test ./internal/archivers/ -run TestInstallGitProtocols -race -count=1`
      passes with no data race
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/archivers/git.go internal/archivers/git_race_test.go`
      prints nothing
- [ ] `git status` shows only `internal/archivers/git.go` modified and
      `git_race_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- The lines 57–62 block in `Archive` does not match the "Current state" excerpt.
- Removing the inline `httpClient` variable causes an "unused variable" or other
  compile error you cannot resolve by deleting only that now-dead local.
- `client.Protocols` is not accessible from the test (API changed) — report the
  go-git version and the actual symbol.

## Maintenance notes

- If go-git is upgraded (Plan 017 / dependency work), re-check whether
  `InstallProtocol` became thread-safe; if so this `Once` is still correct and
  harmless. Newer go-git also offers per-clone transport options that would let
  you drop the global entirely — a cleaner future refactor, out of scope here.
- A reviewer should confirm no other code path calls `client.InstallProtocol`
  (grep the repo): `grep -rn "InstallProtocol" --include=*.go .` should show only
  `internal/archivers/git.go`.
