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
