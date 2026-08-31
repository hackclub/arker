# Testing the social-archive contract

The social-archive contract says a recognized post URL yields a durable, true
archive: every obtainable media asset, normalized metadata, the sanitized raw
extractor record, and provenance — and that anything less never reads green.
Every clause of it depends on what `yt-dlp` and `gallery-dl` actually return,
which changes weekly and cannot be reached from CI.

This is the evidence layer that closes that gap: a corpus of sanitized real
extractor output, a fake extractor that replays it, and tests that drive the
real archiver code against it with no network at all.

- **Corpus**: `internal/testfixtures/testdata/`
- **Harness**: `internal/testfixtures/fakeextract.go`
- **Tests**: `contract_*_test.go` in `internal/archivers`, `internal/workers`,
  `internal/handlers`
- **Regeneration**: `scripts/sanitize_fixture.py`,
  `scripts/build_constructed_fixtures.py`

Run everything with `go test ./...`. Nothing here needs a network, a browser,
a Postgres, or a credential.

## Why fake the binary rather than the archiver

`YtDlpArchiver.Archive` calls `exec.CommandContext(ctx, "yt-dlp")` directly.
There is no `var execCommand = exec.Command` seam and no interface between the
archiver and the process, by design — the archiver *is* the argv, the output
scan and the parsing.

Stubbing `archivers.Archiver` skips all of that. It proves the worker handles a
`Result`, which is worth testing and already is, but it proves nothing about
whether Arker asked yt-dlp for the right things, found the files it wrote, or
normalized them correctly. Those are where the contract actually lives.

So the fake goes on `PATH` instead, and everything downstream of the process is
the real code:

```go
testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: "youtube_regular"})

archiver := &archivers.YtDlpArchiver{}          // the real one
result, err := archiver.Archive(ctx, url, &log, nil, 1)
```

`InstallFakeYtDlp` prepends a directory to `PATH` with `t.Setenv`, so the fake
is only visible to that test and is removed on cleanup. Because it uses
`t.Setenv`, these tests cannot be parallel — consistent with the rest of the
repo, which uses no `t.Parallel` anywhere.

## How the fake works

Two pieces, split so the interesting half stays in Go:

1. **The installer (Go)** stages the exact files a real run would have left
   behind into a temp directory: the info JSON, the media, the poster, the
   subtitle tracks; or for gallery-dl, the flat numbered slides and their
   sidecars.
2. **The script (POSIX sh)** parses the handful of flags Arker passes, copies
   the staged files into place, and exits with the requested status.

The script uses only `cp`, `cat`, `basename` and `sed`, so it behaves the same
on macOS and Linux. It answers every way Arker invokes the tools:

| Invocation | Caller | Fake behavior |
| --- | --- | --- |
| `--version` | `utils.YtDlpVersion`, `utils.GalleryDlVersion` | prints a fixed version |
| `--print title,duration,uploader` | the accessibility probe in `ytdlp.go` | prints the fixture's values |
| `--print duration --skip-download` | `utils.ProbeYtDlpDuration` | prints the fixture's duration |
| `-o <base>.%(ext)s` | the yt-dlp download run | writes `<base>.mp4`, `.info.json`, `.jpg`, `.en.vtt` |
| `-D <dir>` | the gallery-dl run | writes `001.jpg`, `001.jpg.json`, … into `<dir>` |

Failure is first class, because most of the contract is about it:

```go
YtDlpFake{Fixture: "instagram_reel", FailProbe: true}     // login wall
YtDlpFake{Fixture: "tiktok_video", FailDownload: true}    // refused mid-download
YtDlpFake{Fixture: "youtube_regular", NoInfoJSON: true}   // no auditable record
YtDlpFake{Fixture: "youtube_regular", NoSubtitles: true}  // asked, got nothing
YtDlpFake{Fixture: "youtube_regular", NoThumbnail: true}  // no poster

GalleryDlFake{Fixture: "instagram_carousel", Slides: 3, ExitCode: 4}  // partial carousel
GalleryDlFake{ExitCode: 16}                                          // auth required
GalleryDlFake{ExitCode: 64}                                          // no extractor
```

`GalleryDlFake.ExitCode` is gallery-dl's real exit bitmask, not an enum: 1
unexpected error, 4 extraction/download failed, 8 anti-bot challenge, 16
authentication required, 32 bad input, 64 no extractor, 128 filesystem error.

Media bytes are synthesized, not committed. Images are real JPEGs encoded at
test time by `PlaceholderJPEG`, because both archivers validate and preserve
the first still as the social thumbnail — filler bytes would silently take the
"no usable cover" branch and stop that path from being tested at all. Video payloads are
deterministic filler, since Arker only ever stores and sizes them.

The harness has its own tests in `internal/testfixtures/fakeextract_test.go`.
If the fake stops writing what the real extractor writes, every contract test
goes green for the wrong reason — which is the failure mode this whole layer
exists to prevent.

## The corpus

18 platform/post-type cases, one row per matrix entry in
`docs/social-contract/CONTRACT-GAPS.md`. The registry is `cases` in
`internal/testfixtures/fixtures.go`; the tests iterate it, so adding a row
covers it everywhere at once.

| Platform | Post type | Fixture | Origin |
| --- | --- | --- | --- |
| YouTube | regular video | `ytdlp/youtube_regular.info.json` + `.en.vtt` | live |
| YouTube | Shorts | `ytdlp/youtube_shorts.info.json` + `.en.vtt` | live |
| Vimeo | video | `ytdlp/vimeo_video.info.json` | constructed |
| Instagram | reel | `ytdlp/instagram_reel.info.json` | constructed |
| TikTok | video | `ytdlp/tiktok_video.info.json` + `.en.vtt` | constructed |
| Facebook | video | `ytdlp/facebook_video.info.json` | constructed |
| Instagram | single image | `gallerydl/instagram_image/` | constructed |
| Instagram | 10-slide carousel, video at slide 4 | `gallerydl/instagram_carousel/` | constructed |
| TikTok | photo post | `gallerydl/tiktok_photo/` | constructed |
| X | image post | `gallerydl/x_image/` | constructed |
| X | video post | `gallerydl/x_video/` | constructed |
| Reddit | image submission | `gallerydl/reddit_image/` | constructed |
| Reddit | gallery submission | `gallerydl/reddit_gallery/` | constructed |
| Reddit | v.redd.it video | `gallerydl/reddit_video/` | constructed |
| Bluesky | image post | `gallerydl/bluesky_image/` | live |
| Bluesky | video post | `gallerydl/bluesky_video/` | live |
| Imgur | album | `gallerydl/imgur_album/` | constructed |
| Flickr | photo | `gallerydl/flickr_photo/` | live |

**Live** means a sanitized recording of a real anonymous run of the locally
installed extractor. **Constructed** means reconstructed from the field shapes
Arker's own mapping code and tests document, because the platform cannot be
reached:

- **Instagram** — policy, not capability. Arker never makes live Instagram
  requests from a developer machine; it risks the account and the IP.
- **X, Facebook** — serve nothing to a logged-out client.
- **Reddit** — blocks the `.json` API from datacenter and many residential IPs.
- **TikTok** — bot challenge on every anonymous request.
- **Vimeo** — yt-dlp's extractor now requires credentials.
- **Imgur** — no anonymous listing to discover an album ID from.

Every constructed record carries an `_arker_fixture` marker, so `grep` tells
you which is which. `Case.Origin` says the same thing in Go.

The gallery-dl fixtures are laid out exactly as gallery-dl writes them under
Arker's flags (`-f "{num:>03}.{extension}" --write-metadata`): a flat directory
of `001.jpg` + `001.jpg.json` pairs. Only the sidecars are committed; the
harness synthesizes the media.

## Sanitization rules

Nothing that came off the wire is committed unmodified. `scripts/sanitize_fixture.py`
applies these, and re-running it is idempotent.

1. **Credential-shaped values are replaced, never deleted.** A key whose name
   matches `authorization|cookie|password|proxy|credential|secret|api_key|
   access_token|refresh_token|session_*|visitor_*|*_token` keeps its key and
   gets `SYNTHETIC-<KEY>-NOT-A-REAL-SECRET`. Deleting it would destroy the
   shape the redaction tests need to exercise.
2. **URL query parameters** matching that list, or named
   `sig|sqp|hash|nonce|hdn|sec|tok|_nc_*`, or with any value longer than 24
   characters, get the same treatment. Real parameters (`itag=137`) are short
   and meaningful and survive.
3. **URL path segments too.** googlevideo's HLS manifest URLs have no query
   string at all — `expire`, `ei`, `ip`, `sig` and `lsig` are alternating path
   segments. A sanitizer that only walks query parameters leaves a live
   signature in the file. (Arker's own `SanitizeJSON` has this blind spot; see
   G14 below.)
4. **Embedded credentials** — `scheme://user:pass@host` becomes `scheme://host`.
5. **IP addresses** become `203.0.113.7`, RFC 5737 TEST-NET-3, which cannot
   route.
6. **Long strings** are truncated at 400 characters with a visible marker.
7. **Bulk fields Arker never reads** are truncated, not dropped, so the key and
   its element shape survive: `formats` and `thumbnails` to 2 entries,
   `chapters` to 1, `heatmap` to 0; `automatic_captions` and `subtitles` are
   dropped (the subtitle *files* are separate fixtures).
8. **`formats` entries are chosen, not sliced.** Taking the first two yields
   storyboard tiles, which teach a reader nothing; the script keeps one real
   video format and one real audio format, so the `bestvideo+bestaudio` merge
   Arker asks for is visible.

What is deliberately kept: yt-dlp's `_version.release_git_head` (its own public
commit) and Bluesky's `bafy…`/`bafk…` CIDs (public AT-Protocol content
addresses). Neither is a secret and both are useful provenance.

### Verifying a fixture is clean

```bash
grep -rE 'sig=|signature|expire=|&ip=|token' internal/testfixtures/testdata   # expect only SYNTHETIC-*
python3 - <<'EOF'
import os, re
BLOB = re.compile(r"[A-Za-z0-9_-]{40,}")
for dirpath, _, files in os.walk("internal/testfixtures/testdata"):
    for f in files:
        p = os.path.join(dirpath, f)
        for m in BLOB.finditer(open(p).read()):
            if "SYNTHETIC" not in m.group(0):
                print(p, m.group(0)[:60])
EOF
```

Anything that survives should be a public content address or a tool version. If
it looks like a signature, it is one.

## Regenerating fixtures

**Never point these at Instagram.** The corpus is worth less than the account.

Live capture, one platform at a time:

```bash
# yt-dlp: metadata and subtitles only, no media
yt-dlp --skip-download --write-info-json --write-subs --write-auto-subs \
       --sub-langs en --no-playlist -o "/tmp/raw/%(id)s" "<public URL>"

# gallery-dl: sidecars only, no media
gallery-dl --no-download --write-metadata -D /tmp/raw/<case> "<public URL>"
```

Then sanitize into the corpus:

```bash
python3 scripts/sanitize_fixture.py ytdlp   /tmp/raw/<id>.info.json \
        internal/testfixtures/testdata/ytdlp/<case>.info.json
python3 scripts/sanitize_fixture.py gallery /tmp/raw/<case>/<file>.json \
        internal/testfixtures/testdata/gallerydl/<case>/001.jpg.json
```

Rename gallery-dl's output to the flat `NNN.ext.json` form Arker's flags
produce — `-D` alone does not, because the fixture must match what the archiver
will see, not what an ad-hoc run produced.

Rebuild the constructed fixtures with `python3 scripts/build_constructed_fixtures.py`.
Edit that script rather than the JSON: it is where the provenance of every
invented value is recorded.

Use anonymous, public, stable URLs, and keep the number of live requests small.
Discovery costs a request too — Bluesky and Flickr both have public listing
APIs that avoid guessing at IDs.

After regenerating: run the leak checks above, then `go test ./...`.

## Contract-pending tests

Where the current code violates the contract, the test asserts the *contract*
and stops at a `t.Skip` naming the gap. Everything before the skip is current
behavior that must keep working, so these are not dormant — they are guarding
the ground already held.

```go
// current behavior, asserted normally
if meta.FileCount != 3 { ... }

t.Skip("contract-pending: G1 — partial-carousel completeness is not recorded yet; enable at integration")

// the contract, waiting on the fix
if raw["completeness"] != "partial" { ... }
```

Turning one on means deleting the `t.Skip` line. To find them all:

```bash
grep -rn "contract-pending" --include="*_test.go" .
```

| Gap | What the tests assert once it lands |
| --- | --- |
| **G1** | The manifest records an expected media count and a completeness verdict; a 3-of-10 carousel never reads fulfilled and warns which slides are missing; a complete run whose exit code complained about something else still reads complete; single-media post types are structurally complete. |
| **G3a** | A TikTok `/photo/` URL is recognized and routed, so a page snapshot of one cannot read as a complete archive. |
| **G12** | gallery-dl sidecars are sanitized on the way *into* the ZIP, not only when served. |
| **G13** | yt-dlp is asked for subtitles with a bounded `--sub-langs`; a fulfilled YouTube result exposes transcript text plus links to the timed originals; auto-caption rolling duplication is collapsed so each spoken line appears once; auto and uploader provenance stay distinct; alt text is surfaced per slide; **and the absence of subtitles never blocks fulfillment.** |
| **G14** (new, number provisional) | `SanitizeJSON` redacts path-embedded signatures, not only query parameters. |

### G14, in more detail

Found by running the Shorts fixture through the real archiver.
`sanitizeJSONString` only walks `url.Query()`, and `signedMediaHost` only
widens that to "every query parameter". But googlevideo's HLS manifest URLs
carry `expire`, `ei`, `ip`, `sig` and `lsig` as path segments:

```
https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1786613715/
  ei/czt9auPuMr2FkucP64C6iQY/ip/198.51.100.23/.../sig/AE0s2JYwRQIgaB68...
```

Every YouTube Shorts capture, every livestream, and anything else yt-dlp
resolves to HLS therefore stores the viewer's client IP and a live URL
signature in raw metadata — which `/video/:shortid/raw` serves publicly. The
assertion is in `TestSanitizeJSONRedactsPathEmbeddedSignatures`.

## Postgres-only tests

`FindOrCreateCapture` serializes concurrent callers with
`pg_advisory_xact_lock`, and `queue.go` deliberately skips that lock on SQLite.
Asserting concurrency safety on SQLite would assert something production never
does, so the concurrency test is gated on `ARKER_TEST_POSTGRES_DSN` — the same
gate the repo already uses, and one CI sets:

```bash
docker run -d --rm --name arker-pg -e POSTGRES_USER=user -e POSTGRES_PASSWORD=pass \
           -e POSTGRES_DB=arker -p 55439:5432 postgres:15
ARKER_TEST_POSTGRES_DSN="postgres://user:pass@localhost:55439/arker?sslmode=disable" \
  go test ./internal/workers/ -race -count=1
```

Each run gets a throwaway schema, dropped on cleanup. The serialized
"join rather than duplicate" case runs everywhere, so the rule is not
completely unguarded without Postgres.

## Adding a platform

1. Add a `Case` to `cases` in `internal/testfixtures/fixtures.go`.
2. Add the fixture: capture and sanitize it, or add it to
   `scripts/build_constructed_fixtures.py` if the platform cannot be reached.
3. Run `go test ./internal/archivers/ -run Native`. The matrix tests iterate
   the registry, so the new row is covered without touching a test.
4. If the platform needs new routing, add the API-level recognition assertion
   too — a recognized post that produces only MHTML must never read green.
