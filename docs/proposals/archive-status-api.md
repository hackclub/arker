# Archive Status API Proposal

## Summary

Add a read-only API endpoint for checking the status of an asynchronous archive capture:

```http
GET /api/v1/archive/:shortid
Authorization: Bearer <api_key>
```

The endpoint should be protected by the existing `RequireAPIKey` middleware, just like `POST /api/v1/archive` and `GET /api/v1/past-archives`.

## Endpoint choice

Preferred endpoint:

```http
GET /api/v1/archive/:shortid
```

A capture short ID is the unit returned by `POST /api/v1/archive` and the unit whose `ArchiveItem` rows move independently through `pending`, `processing`, `completed`, and `failed`. Querying by short ID is therefore unambiguous and maps directly to the async work the caller is polling.

Alternative considered:

```http
GET /api/v1/status?url=https://example.com
```

This mirrors `GET /api/v1/past-archives?url=...`, but it is less precise because one URL can have many captures. A URL-based form would need extra semantics such as "latest capture only" or pagination. That can be added later as a convenience endpoint if needed.

## Response schema

Example response:

```json
{
  "short_id": "p9OGi",
  "url": "https://example.com",
  "timestamp": "2026-07-07T12:00:00Z",
  "items": [
    {
      "type": "mhtml",
      "status": "completed",
      "extension": ".mhtml",
      "file_size": 12345
    },
    {
      "type": "screenshot",
      "status": "processing"
    },
    {
      "type": "git",
      "status": "failed"
    }
  ],
  "done": false
}
```

Field mapping:

| JSON field | Source |
| --- | --- |
| `short_id` | `models.Capture.ShortID` |
| `url` | `models.ArchivedURL.Original`, loaded via `Capture.ArchivedURLID` |
| `timestamp` | `models.Capture.Timestamp` |
| `items[].type` | `models.ArchiveItem.Type` |
| `items[].status` | `models.ArchiveItem.Status` |
| `items[].extension` | `models.ArchiveItem.Extension`; include only when non-empty, normally once completed |
| `items[].file_size` | `models.ArchiveItem.FileSize`; include only when greater than zero, normally once completed |
| `done` | `true` when every archive item is in a terminal state (`completed` or `failed`) |

`Extension` and `FileSize` are only meaningful after an item reaches `completed`. Pending, processing, and failed items may omit them.

## Type naming

Expose the API's canonical archive type strings: `mhtml`, `screenshot`, `git`, `youtube`, and `itch`.

Reasoning:

- `POST /api/v1/archive` already accepts `types` using the internal/canonical names from `utils.ArchiveRequest.Validate`, including `mhtml`.
- `ArchiveItem.Type` stores those same names, so the response maps directly to model data.
- The web UI's URL layer maps `web` to internal `mhtml` with `urlTypeToInternalType` / `internalTypeToURLType` in `display.go`; that is a display-route concern, not the API contract.

If API consumers later need user-facing labels, add a separate field such as `display_type` rather than changing `type` and breaking consistency with the existing archive request API.

## Auth and errors

- Missing, malformed, inactive, or invalid API key: keep the existing `RequireAPIKey` behavior and return `401` with its current JSON error shape.
- Unknown `shortid`: return `404` with a JSON error, e.g. `{ "error": "archive not found" }`.
- Database errors other than not found: return `500` with a generic JSON error, consistent with existing API handlers.

## Implementation notes

Suggested handler:

```go
func ApiArchiveStatus(c *gin.Context, db *gorm.DB) {
    // shortid := c.Param("shortid")
    // db.Where("short_id = ?", shortid).Preload("ArchiveItems").First(&capture)
    // db.First(&archivedURL, capture.ArchivedURLID)
    // map Capture + ArchiveItems into response structs
}
```

Register next to the existing API routes:

```go
r.GET("/api/v1/archive/:shortid", handlers.RequireAPIKey(db), func(c *gin.Context) {
    handlers.ApiArchiveStatus(c, db)
})
```

Reuse patterns already present in the codebase:

- `internal/handlers/api.go`: response structs and `c.JSON` style from `PastArchiveResponse`, `ApiPastArchives`, and `ApiArchive`.
- `internal/handlers/display.go`: `Preload("ArchiveItems")` capture lookup shape used by the display page.
- `cmd/main.go`: route should be protected with `RequireAPIKey(db)` like `POST /api/v1/archive` and `GET /api/v1/past-archives`.

The first implementation should be read-only. It should not change `POST /api/v1/archive` or `GET /api/v1/past-archives` response shapes.

## Open questions

- Should logs be included, linked, or kept out of the API entirely?
- Should a convenience form such as `GET /api/v1/status?url=...` return the latest capture for a URL?
- Should this endpoint eventually replace the web UI's current `/logs/:shortid/:type` polling for item status?
- Should archive status polling have explicit rate limits or cache headers?
- If captures ever have many item types, should the `items` array need pagination or filtering?
- Should `done` treat skipped/cancelled future statuses as terminal if those statuses are added?
- Should a future completion webhook reuse this exact response schema as its payload?
