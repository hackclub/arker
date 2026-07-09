# Plan 011: Guard the MHTML CDP result parsing against panicking type assertions

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat dcd7526..HEAD -- internal/archivers/mhtml.go`
> If the file changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: 001
- **Category**: bug
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`MHTMLArchiver.Archive` parses the Chrome DevTools `Page.captureSnapshot` result
with two **unchecked** type assertions on one line:
`result.(map[string]interface{})["data"].(string)`. If the CDP call returns a nil
or unexpectedly-shaped result, this panics. River recovers the panic into a job
failure, but it surfaces as an opaque crash-style error and repeats on every
retry for that page, instead of a clean, logged archive error. Extracting the
parse into a small helper with comma-ok assertions removes the panic and makes the
behavior unit-testable without a browser.

## Current state

`internal/archivers/mhtml.go` — the parse (lines 81–88):
```go
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to capture MHTML snapshot: %v\n", err)
		return nil, "", "", bundle, err
	}

	dataStr := result.(map[string]interface{})["data"].(string)
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", bundle, nil
```
`result` is the CDP call's return value (an `interface{}`); `bundle` is the
`*PWBundle` in scope; `fmt` and `strings` are already imported.

## Commands you will need

| Purpose      | Command                                                          | Expected  |
|--------------|------------------------------------------------------------------|-----------|
| Build        | `go build ./...`                                                 | exit 0    |
| Vet          | `go vet ./...`                                                   | exit 0    |
| Unit test    | `go test ./internal/archivers/ -run TestParseMHTMLSnapshot -count=1` | `ok`  |
| Format check | `gofmt -l internal/archivers/mhtml.go`                          | no output |
| Tests (all)  | `go test ./... -count=1`                                         | no `FAIL` |

## Scope

**In scope**:
- `internal/archivers/mhtml.go` (extract + guard the parse)
- `internal/archivers/mhtml_test.go` (create)

**Out of scope**:
- The Playwright/CDP call itself and browser setup — unchanged.
- `internal/utils/mhtml_converter.go` (the MHTML→HTML renderer) — unrelated.

## Git workflow

- Branch: `advisor/011-mhtml-parse-guard`
- Commit message: `Guard MHTML snapshot parsing against bad CDP result shape`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add a guarded parse helper

In `internal/archivers/mhtml.go`, add a package-level helper (near the other
functions in the file):
```go
// parseMHTMLSnapshot extracts the MHTML payload from a Page.captureSnapshot CDP
// result. It returns a descriptive error instead of panicking when the result is
// nil or not the expected {"data": string} shape.
func parseMHTMLSnapshot(result interface{}) (string, error) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("unexpected MHTML snapshot result type %T", result)
	}
	data, ok := m["data"].(string)
	if !ok {
		return "", fmt.Errorf("MHTML snapshot result missing string \"data\" field")
	}
	return data, nil
}
```

### Step 2: Use the helper in `Archive`

Replace the single-line assertion (line 86) and use the helper:
```go
	dataStr, err := parseMHTMLSnapshot(result)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to parse MHTML snapshot result: %v\n", err)
		return nil, "", "", bundle, err
	}
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", bundle, nil
```

Note: `err` is already declared earlier in the function, so `dataStr, err := ...`
is valid (one new variable, `dataStr`, on the left).

**Verify**: `go build ./...` → exit 0; and
`grep -n ".(map\[string\]interface{})\[\"data\"\]" internal/archivers/mhtml.go`
→ returns nothing (the raw double assertion is gone).

### Step 3: Add tests for the helper

Create `internal/archivers/mhtml_test.go`:
```go
package archivers

import "testing"

func TestParseMHTMLSnapshot(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseMHTMLSnapshot(map[string]interface{}{"data": "MIME-body"})
		if err != nil || got != "MIME-body" {
			t.Fatalf("got %q, err %v", got, err)
		}
	})
	t.Run("nil result", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(nil); err == nil {
			t.Fatal("expected error for nil result")
		}
	})
	t.Run("wrong outer type", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot("not a map"); err == nil {
			t.Fatal("expected error for non-map result")
		}
	})
	t.Run("missing data field", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(map[string]interface{}{"other": 1}); err == nil {
			t.Fatal("expected error for missing data field")
		}
	})
	t.Run("data not a string", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(map[string]interface{}{"data": 123}); err == nil {
			t.Fatal("expected error for non-string data")
		}
	})
}
```

**Verify**:
`go test ./internal/archivers/ -run TestParseMHTMLSnapshot -count=1` → `ok`.

## Test plan

- Table test covers: valid shape, nil, wrong outer type, missing `data`, and
  non-string `data`. None of these panic after the fix.
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "parseMHTMLSnapshot" internal/archivers/mhtml.go` shows the helper
      and its call site
- [ ] `grep -n ".(map\[string\]interface{})\[\"data\"\]" internal/archivers/mhtml.go`
      returns nothing
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/archivers/ -run TestParseMHTMLSnapshot -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/archivers/mhtml.go internal/archivers/mhtml_test.go` prints nothing
- [ ] `git status` shows only `mhtml.go` modified and `mhtml_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- Lines 81–88 of `mhtml.go` do not match the "Current state" excerpt.
- `result` is not an `interface{}` (e.g. the CDP binding already returns a typed
  struct), so the helper signature does not fit — report the actual type.

## Maintenance notes

- If the Playwright/CDP binding is upgraded and `captureSnapshot` returns a typed
  result, replace the `interface{}` helper with a typed field access and delete the
  helper's map path.
- Reviewer: confirm the failure path returns the `bundle` (so browser cleanup in
  the worker still runs) — the excerpt does.
