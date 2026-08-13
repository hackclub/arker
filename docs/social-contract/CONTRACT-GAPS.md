# Arker Social-Archive Contract — Platform Matrix & Gap Analysis

Manager: overnight session 2026-08-12/13. Base: origin/main @ 814fd21.
All file:line references are against that commit.

## The contract (non-negotiable)

Given a social-media URL, Arker creates a durable true archive with:
1. **Fulfilled only if complete**: every obtainable source media asset + normalized
   metadata + sanitized raw provider/extractor data + provenance stored. Missing,
   partial, pending, auth-blocked, unsupported, or unknown-completeness must NEVER
   read green/fulfilled.
2. **Find-or-create**: newest completed qualifying archive regardless of age; else
   join qualifying in-flight work; else create once. Concurrency-safe, canonical
   URL/post identity.
3. **Free first**: yt-dlp/gallery-dl/public extraction first. Bright Data only after
   a recorded free-path failure. Model attempts, reasons, backend, request IDs where
   safe, actual cost, aggregates. (No paid calls in this run — mocks only.)
4. **All media**: carousels store all assets with completeness. Videos store actual
   source video+audio, not previews/hotlinks. Ordinary URL behavior stays compatible.
5. **Explicit social intent**: recognized social posts never silently succeed as
   MHTML/screenshot-only.
6. **One stable versioned API** exposing status, normalized post, Arker-hosted media
   URLs, raw metadata, provenance, fulfillment/degradation, cost. Compatible.

## Current architecture (evidence)

- Archive types: mhtml, screenshot, git, yt-dlp, gallery-dl, itch
  (internal/utils/archive_types.go:9-16). Legacy alias "youtube"→"yt-dlp".
- Routing: utils.GetArchiveTypes (internal/utils/url_utils.go:311-336) via
  IsVideoURL/IsGalleryDLURL/ShouldCreateGalleryDLItem.
- Item lifecycle: pending→processing→completed|failed (workers/archive_worker.go).
  saveArchiveResult writes artifact + metadata sidecars BEFORE flipping completed
  (archive_worker.go:183-217). Storage keys nonce-suffixed (bucket lock, no overwrite).
- Unified API: GET /api/v1/archive/:shortid (handlers/archive_result.go). schema_version "1".
  social_post{status,terminal,fulfilled,platform,post,media[],bundle_url,raw_metadata,
  provenance{source,mode},warnings,failure{code,message,retryable}}.
- find-or-create: POST /api/v1/archive/find-or-create → workers.FindOrCreateCapture
  (workers/queue.go:112-238). pg advisory lock on raw URL string; newest completed by
  max(item.UpdatedAt) of required types; else newest in-flight; else create.
- Bright Data fallback wraps yt-dlp/gallery-dl archivers (brightdata/fallback.go):
  native runs first; fallback only if SupportsFallback (IG both; YT if browser zone
  ready) and ≥3min budget left. Usage rows (models.BrightDataUsage) per billable op,
  written for failures too. Cost estimates from env rates. items.source='brightdata'.
- Aliasing (QueueCapture only, freshness window): alias captures own no items; serving
  redirects; find-or-create never creates aliases and skips them as candidates.

## Platform/post-type matrix (claimed today @ 814fd21)

| Platform | Post type | Routed to | Evidence | Status/gap |
|---|---|---|---|---|
| YouTube | regular video | yt-dlp | url_utils.go:100-103 (substring youtube.com/youtu.be) | OK. Any youtube.com URL incl. channel/playlist pages get yt-dlp item (--no-playlist caps damage); non-video YT pages produce failed items — acceptable, but matrix tests should pin single-video behavior |
| YouTube | Shorts | yt-dlp | same (substring match) | OK |
| Vimeo | video | yt-dlp | url_utils.go:106-109 | OK |
| Instagram | Reel (/reel/) | yt-dlp (+BD fallback) | url_utils.go:112-115,165-170 | OK. Anonymous works ~83%; cookies help; BD rescue |
| Instagram | image/carousel (/p/, /tv/) | gallery-dl (+BD fallback) | url_utils.go:117-131,193-211; ShouldCreateGalleryDLItem:286-302 | Partial-download false-green (G1). Item skipped when no cookies & no BD → explicit authentication_required in API (archive_result.go:236-243) ✓ |
| TikTok | video (/video/, vm/vt/t short links) | yt-dlp | url_utils.go:134-144 | OK for video |
| TikTok | photo post (/photo/) | **NOTHING** | not matched by IsTikTokURL; no galleryDLSites entry | **G3a: unrouted → mhtml/screenshot only, social_post=null → silent non-social. Violates #4/#5** |
| Facebook | reel//videos//watch/fb.watch | yt-dlp | url_utils.go:147-158 | Claimed shapes only; FB photo posts unclaimed (ordinary URL) — document |
| X/Twitter | /status/ (image or video) | gallery-dl, requiresCookies | url_utils.go:197-198 | No cookies → no item + explicit auth failure ✓. With cookies gallery-dl fetches images AND videos. No yt-dlp path. Verify video bytes via fixture (G3b) |
| Reddit | /comments/, redd.it (image/gallery/video) | gallery-dl | url_utils.go:200-201 | **G3c: v.redd.it videos are DASH with separate audio; gallery-dl needs ytdl integration for muxed output — verify + fix, else source video/audio violated** |
| Bluesky | /post/ image/video | gallery-dl | url_utils.go:203 | Verify video blob download via fixture (G3d) |
| Pinterest | /pin/ | gallery-dl, requiresCookies | url_utils.go:199 | advertised; test |
| Tumblr | /post/ | gallery-dl | url_utils.go:202 | advertised; test |
| Flickr/Imgur/DeviantArt/ArtStation/Pixiv/Newgrounds/VSCO | post shapes | gallery-dl | url_utils.go:204-210 | advertised; fixture tests at least for imgur+flickr |
| Git hosts | repo | git | url_utils.go:27-67 | ordinary behavior — keep compatible |
| itch.io | game page | itch | url_utils.go:305-308 | ordinary behavior |

## Gaps (G-numbers are work-item IDs)

- **G1 (critical, false-green)**: gallery-dl keeps partial downloads and marks item
  completed (internal/archivers/gallery_dl.go:160-172 "keeping partial archive").
  GalleryMetadata has FileCount but no expected count (gallery_dl.go:38-55). API
  fulfillment = validPost && len(media)>0 (handlers/archive_result.go:265-266) → a
  3-of-10 carousel reads fulfilled. FIX: detect expected count (gallery-dl sidecar
  "count" key where extractor provides; else unknown), record completeness on the
  item (additive column: complete|partial|unknown), propagate to metadata.json,
  social_post (partial ⇒ never fulfilled; warnings list missing indices), viewer badge.
  A partial run whose runErr!=nil but count complete = complete. Unknown ⇒ not fulfilled
  (unknown-completeness never green) unless single-media post types where count is
  structurally 1 (yt-dlp video) — those are complete when the artifact stored.
- **G2 (fulfillment inputs)**: fulfilled must also require retrievable raw metadata
  (RawMetadataKey for yt-dlp; raw sidecars in gallery zip) + provenance. Legacy
  archives lacking sidecars stay explicit non-fulfilled (legacy_archive) ✓ exists.
- **G3 (routing claims)**: a=TikTok /photo/ → route to gallery-dl (gallery-dl supports
  tiktok); short links stay yt-dlp but photo-shaped failures must be explicit, never
  silent. b=X video fixture proof. c=Reddit v.redd.it audio+video (enable/verify
  gallery-dl ytdl integration or dual-route to yt-dlp). d=Bluesky video fixture proof.
  Recognition (archive_result.go:224) must recognize every claimed shape incl. TikTok
  photo so social_post is never null for them (compat: null→failed object is the
  contracted improvement).
- **G5 (canonical identity)**: find-or-create matches raw URL string only
  (workers/queue.go:127-128); advisory lock on raw string. youtu.be/X vs watch?v=X,
  x.com vs twitter.com vs mobile.*, IG ?igsh= tracking params, trailing slashes → all
  distinct identities today. FIX: canonical URL function for RECOGNIZED social
  platforms only (never rewrite ordinary URLs); additive canonical_url column on
  archived_urls + backfill; lookup/lock by canonical; keep original for display.
- **G7 (structured provenance/attempts)**: attempts count exists (RetryCount), reasons
  in logs only; BD usage rows have product/dataset/snapshot/cost. Surface in
  social_post.provenance: attempts, last_failure_reason (sanitized), fallback ops
  summary (product, records, bytes, snapshot_id, est cost). No secrets/signed URLs.
- **G9 (canaries)**: none exist. Build cost-aware recurring canaries + reporting
  (design doc in this folder when written). NOT activated this run.
- **G11 (multi-item social)**: buildSocialPost picks first yt-dlp-or-gallery-dl item
  (archive_result.go:230-235); prefer the media item that matches the URL's routed
  type; if both exist prefer completed one.
- **G13 (metadata maximalism — Zach, 2026-08-12 ~23:57 ET)**: "I give you a social
  media post, you get me everything you can from it within reason." Priority order:
  (a) YouTube: transcripts + subtitles — manual subs + auto-captions (original
  language + English; env-tunable langs), stored as durable artifacts, exposed in
  normalized metadata + unified API (subtitle links + plain-text transcript);
  chapters and other info.json extras surfaced where present. (b) Other yt-dlp
  platforms (TikTok, Vimeo, Facebook): same mechanism, gracefully absent when the
  extractor exposes none. (c) Instagram Reels: attempt subs natively (usually not
  exposed); check Bright Data reels dataset for transcript fields and map when
  present; explicitly OK to report "not obtainable". (d) gallery platforms:
  surface per-file alt text in normalized metadata where extractors provide it.
  Never fail an archive over missing subtitles; absence is recorded, not red.
- **G12 (raw sidecar sanitization at rest)**: /gallery/:id/raw sanitizes at serve
  time (gallery_dl_serve.go:266) but the public zip bundle stores gallery-dl
  sidecars UNsanitized (gallery_dl.go writeGalleryZip) while yt-dlp raw is
  sanitized before storage (ytdlp.go:167 BuildYtDlpVideoArtifacts). Fix: sanitize
  .json sidecars during zip writing (archivers.SanitizeJSON). Manager will handle
  at integration if not covered by an agent.

## Guardrails for all agents (this run)

- NO git push (any remote), no deploy, no prod access (no ssh), no Coolify, no paid
  Bright Data calls (mock/fixture only), no secrets in code or logs.
- No live Instagram requests (account/IP throttle risk — fixtures only). Other
  platforms: keep live requests to a small handful, anonymous only.
- Tests must be deterministic/offline (fake extractor binaries + sanitized fixtures).
- DB migrations: additive only, safe on existing prod data, idempotent.
- Keep ordinary (non-social) URL behavior and existing API fields compatible.
  /api/v1/archive/:shortid stays schema_version "1" with additive fields only.
- Commit locally to your assigned branch with clear messages. Do not merge main into
  your branch mid-run; the manager integrates.

## Integration

Manager branch: fulfill-social-contract (worktree
/Users/zrl/.paseo/worktrees/0edrga46/social-contract-manager). Ledger:
docs/social-contract/LEDGER.md.
