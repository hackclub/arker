# Plan 001: Make `make test` run the whole suite and add a CI gate before deploy

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- Makefile AGENT.md`
> If either file changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch,
> treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: dx
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`make test` runs `go test -v` with no package argument, so it compiles and runs
**only the root package** (`monitoring_test.go`, `storage_test.go`,
`validation_test.go`). Every test under `internal/handlers`, `internal/storage`,
`internal/utils`, `internal/archivers`, and `cmd/` — the majority of the suite —
is silently skipped. A contributor who runs `make test` sees green while never
exercising the handler, storage, or worker tests. Combined with the fact that
there is **no CI at all** and production auto-deploys on every push to `main`
(Coolify), a regression that compiles but fails tests reaches
`archive.hackclub.com` unchecked. This plan makes the local command honest and
adds a CI job that gates pushes/PRs. It is a prerequisite for the bug-fix plans
that follow, whose new tests only pay off if the suite actually runs.

## Current state

- `Makefile` — the `test` target (lines 36–38):
  ```make
  test:
  	@echo "🧪 Running tests..."
  	go test -v
  ```
- `AGENT.md` line 16 documents the same command:
  ```
  - **Test**: `go test -v`
  ```
- There is **no** `.github/` directory. Confirm with:
  `ls .github 2>/dev/null || echo "no .github"` → prints `no .github`.
- The whole suite already passes today when invoked correctly. Verified:
  `go test ./... -count=1` → every package prints `ok` or `[no test files]`,
  no `FAIL`.
- One Postgres-only integration test skips unless a DSN is provided:
  `internal/utils/archive_item_logs_postgres_test.go:18` reads
  `os.Getenv("ARKER_TEST_POSTGRES_DSN")` and `t.Skip`s when unset. Wiring a
  Postgres service in CI (below) lets it run.
- Go version comes from `go.mod`: `go 1.24.4`, `toolchain go1.24.5`.

## Commands you will need

| Purpose       | Command                     | Expected on success                       |
|---------------|-----------------------------|-------------------------------------------|
| Build         | `go build ./...`            | exit 0, no output                         |
| Vet           | `go vet ./...`              | exit 0, no output                         |
| Format check  | `gofmt -l .`                | no output (no files listed)               |
| Tests (all)   | `go test ./... -count=1`    | every package `ok`/`[no test files]`, no `FAIL` |

## Scope

**In scope** (the only files you should modify or create):
- `Makefile` (edit the `test` target)
- `AGENT.md` (update the one Test-command line)
- `.github/workflows/ci.yml` (create)

**Out of scope** (do NOT touch):
- Any `*_test.go` file. Do not "fix" the network-dependent validation tests here
  — that is a separate, unselected finding. If they fail in CI, see STOP conditions.
- Any source file under `cmd/` or `internal/`.
- The `docker-compose*.yml` / `Dockerfile*` files.

## Git workflow

- Branch: `advisor/001-verification-baseline`
- One commit is fine; message style matches the repo's short imperative log
  (e.g. `Run full test suite in make test; add CI workflow`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Fix the `make test` target

In `Makefile`, change the `test` target's command from `go test -v` to
`go test ./... -count=1`. Keep the `@echo` line. Result:

```make
test:
	@echo "🧪 Running tests..."
	go test ./... -count=1
```

**Verify**: `make test` → runs tests in all packages (you will see lines for
`arker`, `arker/cmd`, `arker/internal/handlers`, `arker/internal/storage`,
`arker/internal/utils`, `arker/internal/archivers`), ending with no `FAIL`.

### Step 2: Update AGENT.md

In `AGENT.md` line 16, change:
```
- **Test**: `go test -v`
```
to:
```
- **Test**: `go test ./... -count=1`
```

**Verify**: `grep -n "go test ./... -count=1" AGENT.md` → returns line 16.

### Step 3: Create the CI workflow

Create `.github/workflows/ci.yml` with exactly this content:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

jobs:
  build-test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:15
        env:
          POSTGRES_USER: user
          POSTGRES_PASSWORD: pass
          POSTGRES_DB: arker
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U user"
          --health-interval 10s
          --health-timeout 5s
          --health-retries 5
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
      - name: Gofmt
        run: test -z "$(gofmt -l .)"
      - name: Test
        env:
          ARKER_TEST_POSTGRES_DSN: postgres://user:pass@localhost:5432/arker?sslmode=disable
        run: go test ./... -count=1
```

**Verify**: `test -z "$(gofmt -l .)"` → exit 0 (the repo is already gofmt-clean;
this proves the CI's gofmt gate will pass on the current tree). Also confirm the
YAML is well-formed: the file exists and `grep -c "run:" .github/workflows/ci.yml`
returns `4`.

## Test plan

No new Go tests. Verification is the commands above plus one full local run:
`go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1` →
build/vet exit 0, `gofmt -l .` prints nothing, tests end with no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "go test ./... -count=1" Makefile` returns the `test` target line
- [ ] `grep -n "go test ./... -count=1" AGENT.md` returns line 16
- [ ] `.github/workflows/ci.yml` exists and `gofmt -l .` prints nothing
- [ ] `go test ./... -count=1` exits 0 with no `FAIL`
- [ ] `git status` shows only `Makefile`, `AGENT.md`, and
      `.github/workflows/ci.yml` changed/created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `go test ./... -count=1` fails locally on a test that appears to make a live
  network call (look for `example.com`, DNS, or HTTP timeouts in the failure —
  this is the known-flaky `validation_test.go`, a separate finding). Do NOT edit
  the test or add skips to hide it; report which test failed and stop.
- The `Makefile` `test` target no longer matches the "Current state" excerpt.
- Any build/vet error appears that is unrelated to your three edits.

## Maintenance notes

- The CI's Postgres service enables the DSN-gated test in
  `internal/utils/archive_item_logs_postgres_test.go`. Plan 014 (worker
  characterization tests) and any future Postgres-only test will run here for free.
- The `validation_test.go` suite makes real network requests; on GitHub-hosted
  runners this normally passes but can flake offline. A separate (currently
  unselected) finding proposes injecting the HTTP client to remove that
  dependency — do that before relying on CI for airtight determinism.
- A reviewer should confirm the Coolify deploy is (or becomes) gated on this
  workflow succeeding; wiring the deploy trigger to require CI is an operator
  action outside this repo.
