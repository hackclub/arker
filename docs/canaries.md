# Production canaries — runbook

Canaries archive a handful of known-good public posts on a schedule and check
the **whole social archive contract** on each result. They exist to answer one
question continuously: *if a user submitted this URL right now, would Arker
produce a real archive or a green-looking lie?*

They are **built but not activated**. Nothing recurring runs until
`CANARY_SCHEDULE` is set. This document is what to do when you want to turn
them on.

---

## TL;DR — activation and rollback

**Activate** (one variable, one restart):

```
CANARY_SCHEDULE=6h
```

Set it in Coolify on the Arker resource and redeploy/restart the app. That is
the whole change. Everything else already ships with a safe default.

**Roll back**: unset `CANARY_SCHEDULE` (or set it to `off`) and restart. No
periodic job is registered, no sweep can start, and the recorded history stays
readable at `GET /admin/canaries`.

**Before you activate**, run the pre-flight in
[Pre-activation check](#pre-activation-check) — it takes one request and it is
the difference between a canary that reports Arker's health and one that
reports six rotted probe URLs.

---

## What a canary actually checks

Each probe archives its URL through the production archive path — the same
`processArchiveJob` the River worker runs, the same storage keys, the same
sidecar-before-completed ordering — and then validates, in order:

| Stage | What must be true |
| --- | --- |
| `routing` | The URL still routes to the expected media archiver (`yt-dlp`/`gallery-dl`). A recognized post that loses its extractor would archive as page-only, which the contract forbids. |
| `capture` | The capture and its archive item were created. |
| `archive` | The archiver returned without error **on the free native path**. |
| `item_completed` | The item is `completed` and has a storage key. |
| `media` | Real bytes of the expected kind are in storage, above the probe's byte floor, and **at least as many assets as the post is known to have**. That count is the partial-download check: gallery-dl records what it downloaded, not what the post contains, so a 2-of-4 carousel is internally consistent and only a pinned expected count catches it. |
| `metadata` | Normalized metadata exists, parses, identifies a post, declares a content type, and has an RFC3339 `archived_at`. For galleries the recorded file count must also not exceed what is really in the bundle. |
| `raw_metadata` | The sanitized provider record is retrievable (sidecar key for yt-dlp, raw `.json` sidecars in the gallery ZIP). |
| `provenance` | The artifact came from `native`, and **no** `bright_data_usages` rows are attributed to the capture. |

A pass records `stage_reached = passed`. A failure records the last stage that
succeeded plus the stage and reason that stopped it, so the row alone tells you
where to look.

---

## Cost

**Money: $0.** Canaries run on an archiver map built before the Bright Data
wrappers are applied, so a probe holds no reference to anything billable. Three
independent guards enforce it:

1. **Structural** — the runner is handed the native-only archiver map.
2. **Assertion** — `AssertNativeOnly` runs at the start of every sweep and
   aborts it if any archiver in scope declares `PaidFallbackEnabled()`.
3. **Detection** — validation fails any probe with a `bright_data_usages` row
   attributed to its capture, so spend arriving by some other route turns the
   probe red instead of passing quietly.

A native failure that production *would* have rescued with a paid fallback is
reported as a **native-path failure**, because that is exactly what it is. The
canary's job is to tell you the free path broke, not to buy its way to green.

Paid probes are a separate opt-in (`CANARY_ALLOW_PAID_FALLBACK`, default
`false`) with per-run and per-day USD ceilings. **They are not part of this
activation** and should stay off.

**Bandwidth and storage: not zero, and worth a decision.** Every sweep writes
real media to the archive bucket, which is append-only — canary artifacts can
never be deleted. Rough per-sweep totals for the default probe set:

| Probe | Approx. per sweep |
| --- | --- |
| `youtube/video` ("Me at the zoo", 18s, 240p only) | ~1 MB |
| `youtube/short` (NASA Artemis I Short) | ~10–30 MB |
| `vimeo/video` ("The Mountain", 3 min, high-bitrate timelapse) | **~150–400 MB** |
| `reddit/gallery` (4 images) | ~5 MB |
| `bluesky/post` (1 image) | ~1 MB |
| `imgur/album` (2 images) | ~7 MB |
| **Total** | **~200–450 MB** |

At `6h` (4 sweeps/day) that is roughly **1 GB/day, ~30 GB/month, permanently**,
and about 90% of it is the Vimeo probe. Three ways to spend less, in order of
preference:

- Point the Vimeo slot at a short clip:
  `CANARY_PROBE_URL_VIMEO_VIDEO=https://vimeo.com/<short-video>` (rotation
  guidance below). This keeps full platform coverage.
- Run `CANARY_SCHEDULE=12h` instead. Halves everything; a broken platform is
  still caught the same day.
- Drop the slot: `CANARY_PROBES=youtube/video,youtube/short,reddit/gallery,bluesky/post,imgur/album`.
  Least preferred — Vimeo is the only non-YouTube yt-dlp coverage, so dropping
  it means a YouTube-specific fix could look like a healthy fleet.

---

## Schedule choice

`CANARY_SCHEDULE` takes a Go duration: `6h`, `12h`, `90m`. Empty, `off`,
`disabled`, `none`, and `0` all mean disabled. Values below **15 minutes** are
rejected — canaries pull real media from other people's platforms, and a
tight loop is indistinguishable from abuse.

**Recommended: `6h`.** Four sweeps a day catches a platform breakage within a
quarter-day, which is faster than user reports and slow enough to stay
invisible in any platform's rate limiting. `12h` is the reasonable frugal
choice (see storage above); anything under `1h` buys almost no detection speed
for a linear increase in permanent storage.

Mechanics: the schedule registers a River **periodic job** (kind
`canary_sweep`) on its own `canary` queue with one worker. `RunOnStart` is
false, so a deploy or a crash-loop cannot turn restarts into a burst of probe
traffic — the first sweep lands one interval after boot. Probes within a sweep
run **sequentially**, and two sweeps can never overlap.

---

## Pre-activation check

Do this **before** setting `CANARY_SCHEDULE`. Manual runs work whether or not
the schedule is on, and cost nothing.

```bash
# 1. Log in once and keep the session cookie.
curl -sS -c cookies.txt -X POST https://archive.hackclub.com/login \
  -d "username=$ADMIN_USERNAME" -d "password=$ADMIN_PASSWORD" -o /dev/null

# 2. Run every probe and wait for the verdicts.
curl -sS -b cookies.txt -X POST 'https://archive.hackclub.com/admin/canaries/run?wait=1' | jq
```

`?wait=1` blocks until every probe finishes (allow several minutes) and returns
each verdict. Then:

1. **Every probe passes** → set `CANARY_SCHEDULE=6h`, restart, done.
2. **A probe fails at `routing`, or at `media` with "no image or video files",
   or with a 404/410-shaped extractor error** → the probe URL has rotted.
   Rotate it (below) and re-run. Do not activate with a known-red slot: a
   fleet that is red on day one trains everyone to ignore it.
3. **`reddit/gallery` fails with a 403/auth-shaped error** → Reddit blocks its
   `.json` API from many datacenter ranges, and gallery-dl's reddit extractor
   depends on that API. This is an infrastructure fact about the host, not an
   Arker regression. Confirm from the prod host with
   `curl -A 'Mozilla/5.0' 'https://www.reddit.com/r/pics/.json' -o /dev/null -w '%{http_code}\n'`;
   if it 403s, drop `reddit/gallery` from `CANARY_PROBES` and note why, rather
   than shipping a permanently red slot.

If a single platform is genuinely broken (not the probe URL), that is the
canary doing its job before it was even switched on — fix or file it, then
activate.

---

## Probe URLs and rotation

Each slot is one stable, public, anonymous-safe post. All defaults were
verified live at authoring time; see the `Note` field on each probe (surfaced
in `GET /admin/canaries`) for why it was chosen and what to watch for.

| Slot | Default | Override variable |
| --- | --- | --- |
| `youtube/video` | "Me at the zoo" (first YouTube video, 2005) | `CANARY_PROBE_URL_YOUTUBE_VIDEO` |
| `youtube/short` | NASA, Artemis I launch Short | `CANARY_PROBE_URL_YOUTUBE_SHORT` |
| `vimeo/video` | "The Mountain", TSO Photography (2011) | `CANARY_PROBE_URL_VIMEO_VIDEO` |
| `reddit/gallery` | 4-image r/interestingasfuck gallery (2021) | `CANARY_PROBE_URL_REDDIT_GALLERY` |
| `bluesky/post` | @bsky.app "first 10 million users" (2024) | `CANARY_PROBE_URL_BLUESKY_POST` |
| `imgur/album` | 2-image account-owned album (2018) | `CANARY_PROBE_URL_IMGUR_ALBUM` |
| `tiktok/video` | BBC News clip — **default-disabled** | `CANARY_PROBE_URL_TIKTOK_VIDEO` |
| `instagram/reel` | placeholder — **default-disabled, cookies-required** | `CANARY_PROBE_URL_INSTAGRAM_REEL` |
| `instagram/carousel` | placeholder — **default-disabled, cookies-required** | `CANARY_PROBE_URL_INSTAGRAM_CAROUSEL` |

Rotation is a config change, not a deploy: set the override variable and
restart.

**Rotating a multi-asset slot needs two variables.** Each probe pins how many
assets its post has, and a bundle with fewer fails as a partial download. If
the replacement post has a different number of images, set the count too:

```
CANARY_PROBE_URL_REDDIT_GALLERY=https://www.reddit.com/r/pics/comments/<new>/
CANARY_PROBE_MEDIA_COUNT_REDDIT_GALLERY=6
```

Current counts are visible per probe at `GET /admin/canaries`
(`min_media_count`, with the variable name in `media_count_override_env`).

**How to pick a replacement.** In order of importance:

1. **Institutional or historically significant.** A government agency, a
   platform's own first-party account, or an upload whose deletion would be
   news. Individual users delete things.
2. **Small.** Every sweep stores it forever. Prefer seconds of video, not
   minutes; prefer a 2-image album to a 138-image one.
3. **Right post shape for the slot.** A Shorts slot needs a *real* Short —
   YouTube redirects `/shorts/<ordinary-video-id>` to `/watch`, so a
   mis-picked ID silently stops testing the Shorts path. Check with
   `curl -sLo /dev/null -w '%{num_redirects} %{url_effective}\n' <url>`:
   a true Short reports `0` redirects.
4. **Anonymous-safe.** No login, no age gate, no region lock. If a slot needs
   cookies it must be marked `RequiresCookies` in the catalog, which keeps it
   out of the default set.
5. **Multi-asset where the slot is about completeness.** `reddit/gallery` and
   `imgur/album` exist to catch partial downloads; a single-image post there
   would test less than the slot is for.

Verify a candidate with a metadata request only (oEmbed, the platform's public
API, or a `HEAD`) — never by downloading the media by hand. Note that Imgur
answers a dead album with **HTTP 200 and its homepage**, so "the URL loads" is
not evidence; check that `og:url` still points at the album.

**Never enable an Instagram probe casually.** Logged out, Instagram serves
nothing, so the probe would report a missing credential as a platform outage
every interval. Logged in, it spends a real account's standing on unattended
traffic against a platform that rate-limits hard. The runner refuses to select
a cookies-required probe when no cookie jar is configured, and says so in the
`skipped` list.

---

## Reading failures

### On the dashboard

```bash
curl -sS -b cookies.txt 'https://archive.hackclub.com/admin/canaries' | jq
```

- `summary` — fleet status: `passing`, `failing`, or `unknown`.
  **`unknown` is not green**: it means no canary has ever reported, which is
  no evidence of health.
- `health` — newest verdict per platform/post-type, with `failure_stage`,
  `failure_reason`, and the capture `short_id`.
- `recent` — raw history rows.
- `probes` — the live probe set with its rotation variables.
- `skipped` — probes that were configured but not run, and why.

Open the failing capture directly at `https://archive.hackclub.com/{short_id}`,
and its extractor logs at `/logs/{short_id}/{type}` — a canary capture is an
ordinary capture in every respect, so every existing debugging tool works on it.

### In the logs

Every failure logs at ERROR with the message **`CANARY FAILED`** and structured
fields: `probe`, `platform`, `post_type`, `url`, `short_id`, `stage_reached`,
`failure_stage`, `failure_reason`, `provenance`, `cost_usd`, `duration_ms`,
`trigger`. Passes log at INFO as `Canary probe passed`.

### In `/health`

`GET /health` gains two additive fields:

```json
{
  "status": "healthy",
  "degraded": true,
  "canaries": {
    "status": "failing",
    "passing": 5,
    "failing": 1,
    "failing_probes": ["reddit/gallery"],
    "last_run_at": "2026-08-12T18:00:04Z",
    "schedule_enabled": true,
    "schedule": "6h"
  }
}
```

The **HTTP status stays 200** and `status` stays `healthy` on purpose. `/health`
is the container's liveness probe: a YouTube-side player change must not put
Arker into a restart loop. Degradation is reported in the body for anything
that looks.

### Triage order

1. Is it one slot or all of them? All slots failing at `archive` usually means
   the host lost egress, yt-dlp/gallery-dl broke, or a dependency vanished —
   check `/health` and the startup health checks first.
2. Did it fail at `routing`? The URL stopped being recognized. Either the probe
   URL changed shape or `GetArchiveTypes` regressed — the second is a real bug.
3. Did it fail at `media`/`metadata`/`raw_metadata`? The extractor "succeeded"
   but the archive is incomplete. **This is the highest-value failure the
   canary produces**: it is a false green reaching real users right now.
4. Did it fail at `provenance`? Either a paid fallback ran (it should be
   impossible) or spend was attributed to a canary capture. Treat as urgent:
   the cost guard is the thing that keeps this system free.
5. Is the probe URL simply gone? Rotate it. Do not "fix" it by lowering a
   media floor.

---

## Running by hand

```bash
# Everything, detached (202, results land in /admin/canaries):
curl -sS -b cookies.txt -X POST 'https://archive.hackclub.com/admin/canaries/run'

# One platform, blocking:
curl -sS -b cookies.txt -X POST 'https://archive.hackclub.com/admin/canaries/run?platform=youtube&wait=1' | jq

# One slot:
curl -sS -b cookies.txt -X POST 'https://archive.hackclub.com/admin/canaries/run?platform=youtube/short&wait=1' | jq
```

Manual runs work whether or not `CANARY_SCHEDULE` is set — an operator asking
for a free, native sweep is always honored. Overlapping sweeps are refused with
`409`.

---

## Alert hookup

The alerting surface today is **structured logs**, which is what Arker already
has. There is no external alerting integration, and this change deliberately
did not invent one.

To hook it up, alert on the log line `CANARY FAILED` at ERROR from the Arker
container (any log drain that can match a substring will do — the message is a
fixed string, and the structured fields carry the detail). Route it wherever
Arker's other production alerts go.

The second, pull-based option: poll `GET /health` from an external uptime
checker and alert when `.canaries.status == "failing"` or `.degraded == true`.
That needs no log pipeline, and it is unauthenticated. Do **not** alert on the
`/health` status code — it stays 200 by design.

---

## Configuration reference

| Variable | Default | Meaning |
| --- | --- | --- |
| `CANARY_SCHEDULE` | *(empty)* | Sweep interval as a Go duration (`6h`). Empty/`off` disables recurring canaries entirely. Minimum `15m`. |
| `CANARY_PROBES` | *(empty)* | Comma-separated probe keys to run, e.g. `youtube/video,bluesky/post`. Empty means every default-enabled probe. |
| `CANARY_PROBE_URL_<SLOT>` | *(built-in)* | Override one probe's URL, e.g. `CANARY_PROBE_URL_VIMEO_VIDEO`. |
| `CANARY_PROBE_MEDIA_COUNT_<SLOT>` | *(built-in)* | Override how many assets that probe's post has. Set it whenever you rotate a multi-asset slot. Non-numeric or non-positive values are ignored so a typo cannot silently disable the check. |
| `CANARY_PROBE_TIMEOUT` | `15m` | Ceiling for a single probe. |
| `CANARY_ALLOW_PAID_FALLBACK` | `false` | Lets probes reach the Bright Data fallback. **Not part of activation.** |
| `CANARY_MAX_COST_USD_PER_RUN` | `0.25` | Per-sweep spend ceiling; only meaningful with paid probes enabled. |
| `CANARY_MAX_COST_USD_PER_DAY` | `1.00` | Daily canary spend ceiling, measured against `bright_data_usages` rows attributed to canary captures (UTC day). |

A malformed `CANARY_SCHEDULE` logs an error and leaves canaries **off** rather
than failing startup — an optional monitoring subsystem must never be able to
keep the archiver from booting.

---

## Data and side effects

- **`canary_runs`** (new, additive) holds one row per probe attempt: slot, URL,
  trigger, timing, stage reached, pass/fail, failure stage and reason, capture
  short ID, media bytes and count, content type, provenance, and cost. The row
  is written *before* the archive starts, so a probe killed mid-run leaves a
  row with no `finished_at` — visibly different from both a pass and a clean
  failure.
- **Canary captures are real captures.** They appear in the admin archive list
  and in `past-archives` for their URL, and they are `force`-created (never
  aliased onto an existing capture), because validating a reused archive would
  test nothing. They have no API key attached; join `canary_runs.short_id` to
  identify them.
- Only the media item is captured per probe (`yt-dlp` *or* `gallery-dl`), not
  MHTML/screenshot: the social contract is what is being checked, and browser
  captures would multiply the cost for no added signal.
