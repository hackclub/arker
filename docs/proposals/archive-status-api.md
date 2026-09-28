# Unified Archive Result API

Status: implemented.

`GET /api/v1/archive/:shortid` is API-key protected and returns schema version
`1`. It resolves aliases internally, includes every archive item, and returns a
provider-neutral `social_post` object for recognized social URLs. Ordinary URLs
return `"social_post": null`.

```json
{
  "schema_version": "1",
  "short_id": "requested-id",
  "canonical_short_id": "capture-id",
  "source_url": "https://example.com/post",
  "archive_url": "https://archive.hackclub.com/capture-id",
  "submitted_at": "2026-08-12T20:00:00Z",
  "capture_done": true,
  "items": [],
  "cost": {
    "currency": "USD",
    "total_usd": 0,
    "estimated": false,
    "breakdown": [
      {"provider": "native", "operations": 2, "successes": 2, "cost_usd": 0, "estimated": false}
    ],
    "note": "Native archive operations are free. Bright Data costs are estimates computed from configured rates; the Bright Data dashboard is the invoice of record."
  },
  "social_post": null
}
```

The cost total includes every Bright Data usage row associated with the
canonical capture, including failed attempts that may still be billable. It is
grouped by Bright Data product and includes operation/success counts plus
records or transferred bytes where applicable. Native work is explicitly
reported at zero cost.

For social captures, `social_post` contains lifecycle flags, normalized post,
author and supplied engagement fields, Arker-stored media URLs, an optional
gallery bundle URL, sanitized raw metadata links, provenance, warnings, and a
structured failure. Status is `pending`, `processing`, `fulfilled`, `partial`,
or `failed`. `fulfilled` is true only when valid normalized post metadata and at
least one Arker-stored media object are both available.

Video raw metadata continues at `/video/:shortid/raw`. Gallery-dl and Bright
Data sidecars are exposed, sanitized again at read time, through
`/gallery/:shortid/raw`. Existing video manifests, gallery lists, media URLs,
and ZIP downloads remain unchanged.

Known IDs always return HTTP 200, including pending, partial, failed,
authentication-blocked, unsupported, and legacy captures. Unknown IDs return
404. Authentication uses the existing `RequireAPIKey` middleware.

`POST /api/v1/archive` remains additive:

```json
{
  "url": "https://archive.hackclub.com/abc12",
  "short_id": "abc12",
  "result_url": "https://archive.hackclub.com/api/v1/archive/abc12"
}
```

## Machine-readable failure policy

A known archive that could not capture the source still returns **HTTP 200**.
This means the status lookup succeeded, not that capture succeeded. Consumers
must use `social_post.fulfilled` and `social_post.failure`, not HTTP status or
the existence of an MHTML/screenshot item.

Every non-null `social_post.failure` now adds `category`, `reason`, and
`retry`. This is additive within schema version 1; existing `code`,
`message`, `retryable`, lifecycle values, and successful responses remain.
Do not parse `message` or `provenance.last_failure_reason` for control flow.

Example: an offline live event, with no stored media:

```json
{
  "status": "failed",
  "terminal": true,
  "fulfilled": false,
  "post": null,
  "media": [],
  "failure": {
    "code": "content_unavailable",
    "message": "The platform reports that this live event is offline or has not started; retry after it becomes playable, not in a tight loop",
    "retryable": true,
    "category": "source_unavailable",
    "reason": "live_not_ready",
    "retry": {
      "action": "wait_for_source_change",
      "automatic": false
    }
  }
}
```

`terminal` applies to **this capture**, not the permanent lifetime of the
source. Stop polling this short ID when terminal. Reading its GET never
schedules a new attempt. Source unavailability is an expected unsuccessful
outcome; clients can show “Unavailable at source” rather than an application
outage.

### Categories and reasons

| Category | Reasons | Meaning |
|---|---|---|
| `source_unavailable` | `live_not_ready`, `recording_unavailable`, `removed_content`, `source_unavailable` | Source reports no playable media. Generic unavailability does not prove deletion or a worldwide block. |
| `access_required` | `authentication_required`, `private_content` | Arker needs legitimate source access; an ordinary retry cannot supply permissions. |
| `capture_failed` | `extraction_failed`, `availability_unconfirmed`, `fallback_budget_exhausted` | Capture failed or its paid allowance was exhausted; do not infer source deletion. |
| `artifact_incomplete` | `legacy_archive`, `metadata_unavailable`, `media_incomplete`, `completeness_unknown`, `raw_metadata_unavailable` | Arker could not fulfill the stored-media/metadata contract. Preserve any reported partial media. |
| `unsupported` | `unsupported_url` | No supported social capture route. |

Provider “not found” alone maps to inconclusive capture failure, not
`removed_content`. When the final error is exhausted fallback allowance,
`fallback_budget_exhausted` takes precedence. It describes the failed attempt,
not a promise that the allowance is still exhausted when GET is read days later.

### Retry actions

| Action | Consumer behavior |
|---|---|
| `wait_for_source_change` | Settle this attempt as source-unavailable. Submit a new capture only after source availability changes; do not repeatedly buy the same unavailable content. |
| `configure_source_access` | Surface an access requirement to the operator. Credentials and authorized access are configured in **Arker**, not in the consumer. Retry only after that changes. |
| `investigate` | Settle as requiring investigation. Arker must diagnose the extractor or fallback limit before another paid attempt. |
| `repair_archive` | Surface an incomplete archive. Repair/backfill is Arker's responsibility, not consumer-side scraping or media-path construction. |
| `do_not_retry` | Stop automatic submissions for this unsupported input. |

`retry.automatic` is false for these exhausted/terminal outcomes. Arker's
internal attempts have already run; this API does not schedule background
recovery. No retry deadline is invented from old logs.

The legacy `retryable` flag means “another capture could succeed under changed
conditions,” **not** “retry immediately.” New clients should prefer
`retry.action` and `retry.automatic`. For an older server without `retry`,
apply a bounded backoff to `retryable=true`; never loop indefinitely.
For an unknown future category, reason or action, display `message` and stop
automatic resubmission until the client understands the new policy.

Minimal consumer flow:

1. `fulfilled=true`: consume Arker's normalized metadata and media URLs.
2. `terminal=false`: keep polling the same GET with backoff; do not resubmit.
3. `terminal=true`: settle this attempt, retain any partial media, and display
   the structured failure category. Only initiate a new attempt after the
   specified retry action has been satisfied.

No internal manifest reads, provider selection, or consumer scraping are
required.
