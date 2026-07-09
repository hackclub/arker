# Plan 007: Redact the yt-dlp proxy credential before it is written to archive logs

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat dcd7526..HEAD -- internal/archivers/youtube.go internal/utils/ytdlp_cookies.go`
> If either file changed since this plan was written, compare the "Current state"
> excerpts against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: LOW
- **Depends on**: 001
- **Category**: security
- **Planned at**: commit `dcd7526`, 2026-07-07

## Why this matters

`YTDLP_PROXY` is typically a residential/mobile proxy URL with embedded
credentials (`http://user:pass@host:port`). The yt-dlp archiver runs with
`--verbose` and `--proxy <YTDLP_PROXY>`, and directs the process's stdout/stderr
into the archive log. yt-dlp's verbose banner echoes its full argument vector
(including the proxy URL), and proxy connection errors print the proxy URL too.
Those logs are then exposed to **anonymous** users through two paths:

1. `GET /logs/:shortid/:type` — a public, unauthenticated endpoint
   (`display.go:252`, route at `cmd/main.go:533`).
2. The public archive display page renders the same log text server-side
   (`display.go:161-162` populates `targetItem.Logs`, shown at
   `templates/display_type.html:340,348`).

So the proxy credential can be read by anyone who views a video archive's page.
Because the leak reaches users through the server-rendered page too, the correct
primary fix is to **redact the secret at the source, before it is persisted** —
not merely to gate one endpoint. This plan wraps the yt-dlp log stream in a
redactor that removes the configured proxy string.

**Operational note for the maintainer:** the proxy credential in `YTDLP_PROXY`
should be treated as already exposed and **rotated**, independent of this code
change.

## Current state

`internal/archivers/youtube.go` — the download command wiring (lines 87–94):
```go
	outputTemplate := tempBase + ".%(ext)s"
	cmd := exec.CommandContext(ctx, "yt-dlp")
	cmd.Args = append(cmd.Args, ytDlpDownloadArgs(outputTemplate)...)
	cmd.Args = append(cmd.Args, cookieArgs...)
	cmd.Args = append(cmd.Args, utils.YtDlpProxyArgs()...)
	cmd.Args = append(cmd.Args, url)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
```
and `ytDlpDownloadArgs` includes `"--verbose"` (youtube.go:149).

`internal/utils/ytdlp_cookies.go` — the proxy value lives here (lines 12–42):
```go
var (
	// ...
	ytDlpProxyMu  sync.RWMutex
	ytDlpProxyURL string
)

func InitYtDlpProxy(proxyURL string) string { /* stores trimmed ytDlpProxyURL */ }

func YtDlpProxyArgs() []string {
	ytDlpProxyMu.RLock()
	defer ytDlpProxyMu.RUnlock()
	if ytDlpProxyURL == "" {
		return nil
	}
	return []string{"--proxy", ytDlpProxyURL}
}
```

The log persistence path (for context; not edited here): yt-dlp writes to
`logWriter`, which is a `*utils.DBLogWriter` (`archive_worker.go:110`) that stores
chunks in `archive_item_logs`. `GetLogs` and the display page read them back.

## Commands you will need

| Purpose      | Command                                                          | Expected  |
|--------------|------------------------------------------------------------------|-----------|
| Build        | `go build ./...`                                                 | exit 0    |
| Vet          | `go vet ./...`                                                   | exit 0    |
| Unit test    | `go test ./internal/utils/ -run TestRedactingWriter -count=1`    | `ok`      |
| Format check | `gofmt -l internal/utils/ytdlp_cookies.go internal/archivers/youtube.go` | no output |
| Tests (all)  | `go test ./... -count=1`                                         | no `FAIL` |

## Scope

**In scope**:
- `internal/utils/redact.go` (create — the redacting writer + secret accessor)
- `internal/archivers/youtube.go` (wrap the yt-dlp log stream)
- `internal/utils/redact_test.go` (create)

**Out of scope**:
- Do NOT add authentication to `/logs` here. Gating that endpoint breaks the
  public archive page's log view; it is an optional, separate follow-up (see
  Maintenance notes). Redaction closes the credential leak without a UX change.
- The `DBLogWriter` chunking logic in `internal/utils/db_log_writer.go` — unchanged.
- The itch/git/screenshot/mhtml archivers — only yt-dlp receives the proxy.

## Git workflow

- Branch: `advisor/007-redact-ytdlp-proxy`
- Commit message: `Redact yt-dlp proxy credential from archive logs`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add a redacting writer and a secret accessor

Create `internal/utils/redact.go`:

```go
package utils

import (
	"io"
	"strings"
)

// RedactPlaceholder replaces any configured secret substring in log output.
const RedactPlaceholder = "[REDACTED]"

// YtDlpProxyRedactionSecrets returns the substrings that must never appear in
// persisted logs (currently the configured proxy URL, which may embed
// credentials). Returns nil when no proxy is configured.
func YtDlpProxyRedactionSecrets() []string {
	ytDlpProxyMu.RLock()
	defer ytDlpProxyMu.RUnlock()
	if ytDlpProxyURL == "" {
		return nil
	}
	return []string{ytDlpProxyURL}
}

// redactingWriter replaces occurrences of any secret with RedactPlaceholder in
// each write before forwarding to the underlying writer. It operates per-write;
// see the note in NewRedactingWriter about the (low-risk) split-across-writes case.
type redactingWriter struct {
	w       io.Writer
	secrets []string
}

// NewRedactingWriter wraps w so that any of secrets is replaced with
// RedactPlaceholder before being written. When secrets is empty it returns w
// unchanged. yt-dlp prints the proxy URL as a single contiguous token on one
// line, so per-write replacement removes it in practice; this is defense at the
// source so the credential is never persisted.
func NewRedactingWriter(w io.Writer, secrets []string) io.Writer {
	nonEmpty := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return w
	}
	return &redactingWriter{w: w, secrets: nonEmpty}
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	s := string(p)
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, RedactPlaceholder)
	}
	if _, err := r.w.Write([]byte(s)); err != nil {
		// Report full consumption of the caller's bytes to avoid short-write retries
		// that would double-log; the redacted content stands in for the original.
		return len(p), err
	}
	return len(p), nil
}
```

Note: `ytDlpProxyMu` and `ytDlpProxyURL` are declared in the same `utils` package
(`ytdlp_cookies.go`), so `YtDlpProxyRedactionSecrets` can read them directly.

**Verify**: `go build ./...` → exit 0.

### Step 2: Wrap the yt-dlp log stream

In `internal/archivers/youtube.go`, just before `cmd.Stdout = logWriter`, create a
redacted writer and use it for both streams. Change lines 93–94 from:
```go
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
```
to:
```go
	redactedLog := utils.NewRedactingWriter(logWriter, utils.YtDlpProxyRedactionSecrets())
	cmd.Stdout = redactedLog
	cmd.Stderr = redactedLog
```

`utils` is already imported in `youtube.go` (it calls `utils.YtDlpProxyArgs()` at
line 91).

**Verify**: `go build ./...` → exit 0; and
`grep -n "NewRedactingWriter" internal/archivers/youtube.go` → returns one line.

### Step 3: Add tests for the redactor

Create `internal/utils/redact_test.go`:

```go
package utils

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactingWriterReplacesSecret(t *testing.T) {
	secret := "http://user:pass@proxy.example.com:8080"
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf, []string{secret})

	line := "[debug] Proxy: " + secret + " connecting...\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked into output: %q", out)
	}
	if !strings.Contains(out, RedactPlaceholder) {
		t.Fatalf("expected placeholder in output: %q", out)
	}
}

func TestRedactingWriterPassthroughWhenNoSecret(t *testing.T) {
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf, nil)
	if w != &buf {
		t.Fatal("expected NewRedactingWriter to return the underlying writer when no secrets")
	}
	msg := "no secrets here\n"
	w.Write([]byte(msg))
	if buf.String() != msg {
		t.Fatalf("passthrough altered output: %q", buf.String())
	}
}
```

**Verify**:
`go test ./internal/utils/ -run TestRedactingWriter -count=1` → `ok`.

## Test plan

- `TestRedactingWriterReplacesSecret`: a line containing the proxy URL comes out
  with the URL replaced by `[REDACTED]`.
- `TestRedactingWriterPassthroughWhenNoSecret`: with no secrets, the wrapper is
  the underlying writer and output is unchanged.
- Full suite: `go test ./... -count=1` → no `FAIL`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `internal/utils/redact.go` exists with `NewRedactingWriter` and
      `YtDlpProxyRedactionSecrets`
- [ ] `grep -n "NewRedactingWriter" internal/archivers/youtube.go` returns one line
- [ ] `go build ./...` and `go vet ./...` exit 0
- [ ] `go test ./internal/utils/ -run TestRedactingWriter -count=1` passes
- [ ] `go test ./... -count=1` exits with no `FAIL`
- [ ] `gofmt -l internal/utils/redact.go internal/utils/redact_test.go internal/archivers/youtube.go`
      prints nothing
- [ ] `git status` shows only `youtube.go` modified and `redact.go` +
      `redact_test.go` created
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- `ytDlpProxyURL`/`ytDlpProxyMu` are not declared in `internal/utils` (the file
  moved), so `YtDlpProxyRedactionSecrets` cannot read them.
- Lines 93–94 of `youtube.go` do not match the "Current state" excerpt.
- You discover another archiver also receives the proxy (grep
  `YtDlpProxyArgs`); if so, wrap its log stream the same way and note it — do not
  leave a second leak path.

## Maintenance notes

- **Split-across-writes limitation**: redaction is per-write. yt-dlp emits the
  proxy URL as one contiguous token on a single line, so in practice it is never
  split, but if a future yt-dlp version streams it byte-by-byte the tail could
  slip through. If you want an airtight guarantee, upgrade the redactor to buffer
  by line (flush complete lines, redact, hold the partial tail) and flush on
  process exit.
- **Optional defense-in-depth**: requiring `RequireLogin` on
  `GET /logs/:shortid/:type` AND removing the server-side `targetItem.Logs`
  rendering from the public display page would make logs admin-only. That is a UX
  change (anonymous users lose the log view) and was deliberately left out of this
  plan. Consider it if logs are judged operator-only.
- Reviewer: confirm the proxy value cannot reach logs by any other route (e.g. a
  future `fmt.Fprintf(logWriter, ...)` that interpolates the proxy).
