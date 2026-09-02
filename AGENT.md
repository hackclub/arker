# Arker Development Guide

Arker is a Go-based web archiving server that captures web pages using multiple strategies and provides short URLs for accessing archived content.

## Deployment

**Production URL**: https://archive.hackclub.com  
**Deployment**: Managed via Coolify (deploys `main` on push; app container name starts with the Coolify resource UUID)  
**Debug Access**: `ssh archive-hq-local.selfhosted.hackclub.com` (via cloudflared, see ~/.ssh/config; use `sudo docker ...` on the host)

## Quick Commands

### Essential Development Commands
- **Build**: `go build -o arker ./cmd`
- **Test**: `go test ./... -count=1`
- **Run locally**: `go run ./cmd` or `go run .`
- **Lint/Format**: `go fmt ./...` and `go vet ./...`
- **Build check**: `go build ./...`

### Docker Development
- **Start dev environment**: `make dev`
- **Build dev containers**: `make dev-build`
- **Stop dev environment**: `make dev-down`
- **View dev logs**: `make dev-logs`
- **View dev logs (non-blocking)**: `make dev-logs | head -n 100` or `docker compose -f docker-compose.dev.yml logs --tail=100`
- **Clean all containers**: `make clean`

#### Development Logs
When using Amp with `make dev` running in another window:
- Use `make dev-logs` to see recent logs (will follow and block)
- Use `docker compose -f docker-compose.dev.yml logs --tail=50` for last 50 lines without hanging
- Use `docker compose -f docker-compose.dev.yml logs arker-app --tail=20` for app-only logs

### Database Operations
- **Connect to dev DB**: `make db-connect`
- **Reset dev database**: `make db-reset`

### Production
- **Start production**: `make prod` or `docker compose up -d`
- **Stop production**: `make prod-down`
- **View production logs**: `make prod-logs`

## Environment Setup

### Local Development (Recommended: Docker)
1. `make dev` (starts PostgreSQL + app with live reload)

### Manual Local Development
1. Start PostgreSQL: `docker run -d --name postgres -e POSTGRES_USER=user -e POSTGRES_PASSWORD=pass -e POSTGRES_DB=arker -p 5432:5432 postgres:15`
2. Install Playwright: `go install github.com/mxschmitt/playwright-go/cmd/playwright@latest && playwright install chromium`
3. Install yt-dlp and gallery-dl: `pip install yt-dlp gallery-dl`
4. Run: `go run .`

### Dependencies
- **Go 1.25.12+**
- **PostgreSQL 15**
- **Git** (for repository archiving)
- **Python 3 + yt-dlp** (for video archiving)
- **Python 3 + gallery-dl** (for photo posts and mixed photo/video carousels) —
  must be installed into the **same Python environment as yt-dlp**, which it
  imports as a module (see Platform routing below)
- **Python 3 + itch-dl** (for itch.io game archiving)
- **ffmpeg** (yt-dlp merges separate video and audio streams with it; without
  it every DASH source, reddit included, archives without sound)
- **Playwright + Chromium** (for MHTML and screenshots)

## Project Structure

```
/
├── cmd/main.go              # Application entry point & service setup
├── internal/                # Internal packages (modular architecture)
│   ├── archivers/          # Archive implementations
│   │   ├── archiver.go     # Base archiver interface
│   │   ├── mhtml.go        # MHTML webpage archiving
│   │   ├── screenshot.go   # Full-page screenshot capture
│   │   ├── git.go          # Git repository cloning
│   │   ├── ytdlp.go        # Video downloading via yt-dlp
│   │   ├── gallery_dl.go   # Photo/carousel downloading via gallery-dl
│   │   ├── itch.go         # itch.io game archiving
│   │   ├── pwbundle.go     # Playwright browser/page lifecycle
│   │   └── utils.go        # Shared browser utilities & page loading
│   ├── handlers/           # HTTP handlers
│   │   ├── admin.go        # Admin interface endpoints
│   │   ├── api.go          # REST API endpoints
│   │   ├── auth.go         # Authentication handlers
│   │   ├── display.go      # Archive display pages
│   │   ├── git.go          # Git HTTP backend
│   │   ├── itch_serve.go   # itch.io individual file serving
│   │   ├── gallery_dl_serve.go # gallery-dl ZIP browsing + per-file serving
│   │   ├── gallery_manifest.go # gallery manifest (status, metadata, card URLs)
│   │   ├── thumb.go        # Thumbnail serving + placeholder
│   │   └── serve.go        # File serving with streaming
│   ├── models/             # Database models & types
│   │   └── models.go       # User, ArchivedURL, Capture, ArchiveItem
│   ├── storage/            # Storage interface & implementations
│   │   ├── fs.go           # Filesystem storage
│   │   ├── s3.go           # S3/R2 storage with presigned direct URLs
│   │   ├── direct.go       # DirectURLStorage interface
│   │   └── memory_storage.go # In-memory storage (tests)
│   ├── thumbnail/          # Derived preview images
│   │   └── thumbnail.go    # Preserve social originals or derive page previews
│   ├── monitoring/         # Browser process monitoring
│   ├── utils/              # Shared utilities
│   └── workers/            # Async job processing
│       ├── queue.go        # Job queue management
│       ├── archive_worker.go   # Archive job processing
│       ├── thumbnail_worker.go # On-demand thumbnail backfill
│       └── cleanup_worker.go   # Stuck-job reaper
├── templates/              # HTML templates for web interface
└── Makefile               # Development workflow commands
```

## Core Interfaces & Architecture

### Key Interfaces
- **`Storage`** - Pluggable storage backend (filesystem or S3/R2)
  - Methods: `Writer(key)`, `Reader(key)`, `Exists(key)`, `Size(key)`
  - `SeekableStorage` adds `SeekableReader` for range requests; `DirectURLStorage`
    adds presigned redirects. There is no delete method: the production bucket is
    locked, so objects are written once under a nonced key and never replaced.
  - Objects are stored **uncompressed** — the bytes on disk are exactly what the
    archiver wrote.
- **`Archiver`** - Different archiving strategies
  - Method: `Archive(ctx, url, logWriter, db, itemID) (Result, error)`
  - `Result` carries the artifact reader, extension, content type, the Playwright
    bundle (browser archivers), and an optional derived thumbnail
  - Types: MHTML, Screenshot, Git, yt-dlp, gallery-dl, Itch

### Performance Features
- **Browser Instance Reuse**: Playwright browsers reused across jobs for efficiency
- **Streaming**: All file operations use streaming
- **Async Processing**: Queue-based job processing with configurable worker pools
- **Concurrent Workers**: Default 5 workers (configurable via `MAX_WORKERS`)
- **Browser Monitoring**: Tracks browser processes to prevent memory leaks

### Database Models
- **User**: Admin authentication (default: admin/admin)
- **APIKey**: API authentication with app tracking
- **ArchivedURL**: Original URLs with metadata
- **Capture**: Archive sessions with short IDs (5-char alphanumeric)
- **ArchiveItem**: Individual archive files per type with logs & status
- **Config**: Persistent configuration (e.g., session secrets)

## API Endpoints

### Public API (Requires API Key)
- `POST /api/v1/archive` - Request new archive
  ```json
  {"url": "https://example.com", "types": ["mhtml", "screenshot"]}
  ```
- `POST /api/v1/archive/find-or-create` - Reuse the latest completed canonical archive, join a matching capture in progress, or queue a new capture
- `GET /api/v1/past-archives?url=...` - Get past archives for URL

### Public Access
- `GET /:shortid` - Archive display page with tabs for each type
- `GET /archive/:shortid/:type` - Download specific archive type
- `GET /archive/:shortid/mhtml/html` - View MHTML as rendered HTML
- `GET /git/:shortid` - Git HTTP backend for cloning repositories
- `GET /itch/:shortid/file/*filepath` - Stream individual files from itch.io game archives
- `GET /itch/:shortid/list` - JSON list of files in itch.io game archive
- `GET /gallery/:shortid/manifest` - Gallery capture status, normalized post metadata, and one absolute media URL per card in swipe order (the video manifest's counterpart; what API consumers should use). A post's soundtrack is reported in a top-level `music` block (`status` `stored`/`metadata_only`, title/artist/id, and `audio_url` when the bytes are in the bundle), never as a card: `media_count` is the slide count
- `GET /gallery/:shortid/list` - JSON post metadata + media file list for a gallery-dl archive (viewer-facing, predates the manifest, shape frozen)
- `GET /gallery/:shortid/file/*filepath` - Stream one media file out of a gallery-dl archive
- `GET /video/:shortid/manifest` - Video capture status, normalized post metadata, and archived media URL. `metadata.media_type` is the platform's own delivery format (YouTube reports `short` or `video`), passed through verbatim from the provider record so it always matches `/video/:shortid/raw`; it is absent — never guessed — when the provider names none (Instagram, TikTok, Facebook, and the Apify fallbacks) or when the archive predates the field
- `GET /video/:shortid/raw` - Sanitized raw yt-dlp/Apify provider record
- `GET /video/:shortid/subtitle/:name` - One stored caption track (`name` is `<lang>.<format>`, e.g. `en.vtt`); only tracks the archive's own metadata records are servable
- `GET /video/:shortid/transcript` - Plain-text transcript derived from the best caption track
- `GET|HEAD /thumb/:shortid` - Preview image for a capture (the original social poster/image when available; otherwise a 480x270 JPEG page preview); falls back to an SVG placeholder and queues generation
- `GET|HEAD /thumb/:shortid/:type` - Preview image for one archive type

### Admin Interface (Session Authentication)
- `GET /login` - Admin login page
- `POST /login` - Authentication endpoint
- `GET /` - Admin dashboard with archive management
- `GET /admin/api-keys` - API key management
- `POST /admin/api-keys` - Create new API key
- `POST /admin/url/:id/capture` - Request new capture
- `GET /admin/item/:id/log` - View capture logs

### Health & Monitoring
- `GET /health` - Application and database health check
- `GET /metrics/browser` - Browser monitoring metrics
- `GET /status/browser` - Browser status (leak detection)

### Git Repository Access
```bash
git clone https://archive.hackclub.com/git/{shortid}
```

## Configuration

### Environment Variables
- `DB_URL` - PostgreSQL connection string
- `STORAGE_PATH` - Archive storage directory (default: `./storage`)
- `CACHE_PATH` - Git clone cache directory (default: `./cache`)
- `MAX_WORKERS` - Worker pool size (default: `5`)
- `PORT` - HTTP server port (default: `8080`)
- `GIN_MODE` - Gin framework mode (`debug` for development)
- `ITCH_API_KEY` - itch.io API key for downloading games (required for itch.io archiving)
- `ITCH_DL_PATH` - Path to itch-dl command (default: `itch-dl`)
- `YTDLP_COOKIES_FILE` - Path to a Netscape-format cookies.txt passed to every yt-dlp invocation (required for Instagram video archiving; Instagram refuses media requests from logged-out clients)
- `YTDLP_COOKIES_B64` - Base64-encoded cookies.txt content, written to a temp file at startup (used when `YTDLP_COOKIES_FILE` is unset; convenient for Coolify secrets)
- `YTDLP_PROXY` - Optional proxy URL (e.g. `http://user:pass@host:port`, `socks5://...`) applied to every yt-dlp call. Instagram aggressively rate-limits datacenter IP ranges; a residential/mobile proxy is the reliable way to archive Instagram from a server. yt-dlp itself must also be kept current (installed from the nightly `--pre` channel) since Instagram breaks the extractor frequently.
- `YTDLP_IMPERSONATE` - Optional yt-dlp `--impersonate` target for Instagram/TikTok/Facebook video URLs. Docker images default to `chrome` and install `curl-cffi`; set empty to disable for manual installs without curl-cffi.
- `GALLERYDL_USER_AGENT` - Optional `--user-agent` override for gallery-dl. Leave unset: gallery-dl sets a per-site User-Agent already (Instagram gets a current Chrome UA because it serves lower-quality video to anything else), and this replaces those defaults everywhere.
- `GALLERYDL_SLEEP_REQUEST` - Optional `--sleep-request` override (`"1"`, `"0.5-1.5"`). Leave unset. gallery-dl ships per-site request intervals (Instagram waits a randomized 6-12s between API calls); because `--sleep-request` is a root config key it *replaces* those rather than acting as a floor, so any value below a site's own default makes throttling more likely, not less. Set it only to slow gallery-dl down further.
- `APIFY_API_TOKEN` - Enables the paid media fallback (see "Apify fallback"). Empty disables it entirely: no actor is started, and login-only sites with no cookie jar go back to producing no media item at all.
- `APIFY_RUN_TIMEOUT` - Bound on one actor run (default `10m`); a run still going at the deadline is aborted so it stops billing.
- `APIFY_MAX_RUN_COST_USD` - A run whose platform-reported cost exceeds this (default `0.5`) is logged loudly. It does not stop the archive — the money is already spent — but a misconfigured actor cannot go unnoticed.

- `ARKER_SUB_LANGS` - Optional override for which subtitle tracks yt-dlp fetches, passed to `--sub-langs` verbatim. Leave unset: the default is computed per video as its own language plus English, using **exact** codes. Do not "improve" it to `en.*` — yt-dlp matches these as anchored regexes and YouTube names machine-translated auto-captions `<target>-<source>`, so `en.*` also matches `en-de` ("English from German"); on a video offering ~150 translations that fetched three tracks and earned an HTTP 429. Use `all,-live_chat` to deliberately hoard every translation.
- `LOGIN_TEXT` - Text to display under login form

### Authentication
- **Admin Username**: `admin` (set via `ADMIN_USERNAME`)
- **Admin Password**: `admin` (set via `ADMIN_PASSWORD`)
- **Session Secret**: Auto-generated and stored in database (override with `SESSION_SECRET`)
- **API Keys**: Managed through admin interface

### Security Features
- Session secret automatically generated with cryptographically secure random bytes
- API keys with prefix for identification and hashed storage
- Per-key usage tracking and activation controls


## Testing

### Test Files

Most tests live beside the package they cover (`internal/*/..._test.go`); a few
integration-level ones sit at the repo root (`storage_test.go`,
`monitoring_test.go`, `validation_test.go`). Run `go test ./...` rather than
working from a list here — this section has drifted before.

Two conventions worth knowing:
- DB-backed tests use in-memory SQLite (`gorm.io/driver/sqlite`) with
  `AutoMigrate`, so they need no running Postgres. See `newWorkerTestDB` in
  `internal/workers/archive_worker_test.go` for the pattern.
- Handler tests build a real `gin` engine and drive it with `httptest`, so route
  registration and middleware are exercised too. See `internal/handlers/thumb_test.go`.

### Running Tests
```bash
go test -v                    # Run all tests
go test -v ./internal/...     # Test internal packages
go test -run TestFSStorage    # Run specific test
```

## Key Dependencies

### Core Framework
- **gin-gonic/gin** v1.9.1 - HTTP router and middleware
- **gorm.io/gorm** v1.30.0 - ORM with PostgreSQL driver
- **gin-contrib/sessions** v0.0.5 - Session management

### Archive & Browser
- **mxschmitt/playwright-go** v0.6100.0 - Browser automation
- **go-git/go-git/v5** v5.8.1 - Git operations
- **HugoSmits86/nativewebp** v1.2.0 - WebP encoding (lossless only)
- **golang.org/x/image** v0.44.0 - WebP decoding + high-quality rescaling

### Utilities
- **golang.org/x/crypto** v0.33.0 - Password hashing (bcrypt)
- **golang.org/x/net** v0.34.0 - Network utilities
- **kelseyhightower/envconfig** v1.4.0 - Environment configuration

## Development Workflow

### Making Changes
1. Use `make dev` for live development with hot reload
2. Test changes: `go test -v`
3. Check compilation: `go build ./...`
4. Format code: `go fmt ./...`
5. Run static analysis: `go vet ./...`

### Adding New Archive Types
1. Implement `Archiver` interface in `internal/archivers/`
2. Add the type constant to `internal/utils/archive_types.go` (`canonicalArchiveTypes`)
3. Add to `archiversMap` in `cmd/main.go`
4. Route URLs to it in `utils.GetArchiveTypes`
5. Give it a timeout case in `utils.TimeoutForJobType` (the default is only 2 minutes)
6. Update `contentTypeForArchive` in `internal/handlers/serve.go`
7. Add it to `defaultTypePreference`/`getDisplayName` in `internal/handlers/display.go` and to the content pane in `templates/display_type.html`
8. Add tests

Type names are stable identifiers: they appear in stored rows, in permalinks
(`/{shortid}/{type}`), and in API requests. Renaming one means adding a
`legacyArchiveTypeAliases` entry so old names keep resolving, plus a rename in
`migrateLegacyArchiveTypes`. Never just change the string.

An archiver returns an `archivers.Result` struct, not a list of values, so a new
derived artifact does not churn every archiver's signature. `Result.Thumbnail`
is optional and nil is not an error.

### Thumbnails

Thumbnails are a **derived artifact**, not an archive type — they have no tab,
no permalink type segment, and no entry in `canonicalArchiveTypes`. They live in
five columns on `archive_items` (`thumbnail_key/width/height/status/kind`) and are
served from `/thumb/{shortid}[/{type}]`.

- **Social thumbnails preserve framing, not full resolution**: use the
  provider's poster or the post's first still, keep the complete aspect ratio,
  and downsize the longest edge to at most 480px for row previews. Small source
  images may retain their original bytes and encoding; oversized images are
  encoded as JPEG. Never crop a social preview.
- **Page screenshot previews are derivatives**: crop the page header and scale
  it to a 480x270 JPEG (`internal/thumbnail`). JPEG is used here because the
  only WebP encoder in the tree (`nativewebp`) is lossless-only — it produced a
  ~490KB derivative versus ~35KB for JPEG and panics on some low-colour inputs.
- **Each archiver supplies its own source**, inline, from work it already does:
  | Type | Source | Treatment |
  | --- | --- | --- |
  | `screenshot` | the full-page image it already decoded — no extra browser work | `CropTop` |
  | `yt-dlp` | the platform's own poster via `--write-thumbnail` (YouTube often serves **WebP**) | fit within 480px |
  | `gallery-dl` | the post's first still image, from the temp dir before it is zipped | fit within 480px |
  | Apify video-only gallery | the provider record's published poster when available | fit within 480px |
  | `mhtml`, `git`, `itch` | none — the capture falls back to a sibling item's thumbnail | — |
- **Only page screenshot previews are cropped.** They use `CropTop` because the
  page's identity is its header. Social images keep their complete framing.
- Screenshot previews are lazily backfilled from their stored screenshot
  (`CanDeriveFromArchive`) on the `high_priority` queue. Historical social
  images use the explicit `/admin/backfill-social-thumbnails` operation and a
  one-worker `thumbnail_backfill` queue: it reads the first still from stored
  gallery ZIPs, asks yt-dlp for posters with media downloads disabled, and only
  then uses the capped Apify poster resolver. Duplicate canonical
  URL/type captures share one object. `thumbnail_kind` makes the operation
  resumable: `social_preview` is contract-ready, a <=480px
  `social_original` is also accepted, and `social_fallback` retains an old
  preview only until stored-video frame extraction can replace it.
- Completed legacy videos without structured sidecars use the explicit
  `/admin/backfill-video-metadata` operation and the two-worker
  `video_metadata_backfill` queue. The job makes one bounded yt-dlp
  `--skip-download` request, probes the immutable stored media, writes new
  append-only sidecars, and shares them across duplicate canonical URLs. A
  final attempt first recovers embedded schema/Open Graph facts from the
  sibling MHTML snapshot, then uses a metadata-only Apify fallback for
  covered platforms when the snapshot is insufficient. None of these paths
  downloads the video again.
- **Never generate inline in a request** — a full-page screenshot reaches 60
  megapixels (~240MB decoded) and one dashboard render asks for hundreds.
- **gallery-dl's thumbnail must be built before the ZIP goroutine starts.** That
  goroutine owns `cleanupTmp`, so the downloaded files can vanish once it runs.
- **`thumbnail_status` must be set to `unavailable` for permanent failures**
  (unsupported type, oversized, undecodable). Without it the lazy path
  re-enqueues the same impossible job on every page view.
- **A thumbnail failure must never fail an archive.** The archive is the
  product; the preview is not.
- Keys carry an upload nonce and their real encoding
  (`{shortid}/{type}-{nonce}-thumb.{jpg,png,gif,webp,avif,heic,heif}`) because the bucket
  forbids overwrites and deletes. Regenerating means writing a new object and
  repointing the row.
- `/thumb/{shortid}` is the permanent reported URL. It returns a generated SVG
  placeholder while work is pending and the real image afterward, never signs
  or redirects, and revalidates so the change is visible without changing the
  URL. Unknown/nonexistent captures return 404.

### Platform routing

`utils.GetArchiveTypes` decides which archivers a URL gets, and
`utils.IsSocialMediaPostURL` is the single source of truth for "this is a
social post, not an ordinary page". Keep them consistent: a URL that earns a
yt-dlp or gallery-dl item but is not recognized is served as a plain page
capture, and a recognized URL that earns no media item must produce an explicit
API failure instead of a green MHTML/screenshot-only archive. The full matrix
is pinned in `internal/utils/routing_matrix_test.go`.

Platform quirks worth knowing before changing a route:

- **gallery-dl imports yt-dlp as a Python module** for every `ytdl:` URL it
  produces — reddit videos, Instagram DASH manifests, TikTok audio. A separate
  pipx/uv install of yt-dlp is invisible to it no matter what `PATH` says: it
  logs `Cannot import yt-dlp or youtube-dl` and downloads nothing. The archiver
  watches for that line and fails with an actionable message.
- **reddit** (`v.redd.it`) is DASH with the audio in a separate stream. Arker
  pins `extractor.reddit.videos=ytdl` so yt-dlp's own reddit extractor supplies
  the format list (DASH + HLS + fallback) and merges audio; the default `dash`
  setting can only use what reddit's manifest happens to list.
- **Vimeo** main-site URLs are login-only as of yt-dlp 2026.08.04. The archiver
  fetches `player.vimeo.com/video/<id>` instead (`utils.YtDlpFetchURL`),
  carrying the unlisted-video hash. Player pages carry less metadata: no upload
  date and no engagement counts. Vimeo has **no Apify pathway and should
  not get one**: the Vimeo Videos dataset crawler errors on exactly the
  DRM-protected class the native path cannot fetch either. DRM is cryptographic
  rather than positional, so a different network position buys nothing.
- **TikTok** photo posts (`/photo/`) go to gallery-dl — yt-dlp cannot download a
  slideshow. Videos and `vm`/`vt`/`t` short links stay on yt-dlp; a short link
  that resolves to a photo post fails explicitly rather than being resolved at
  routing time, which would mean a network call from a pure function.
- **Soundtracks on image posts.** Instagram carousels and TikTok slideshows
  carry a music track. gallery-dl writes it as just another numbered file
  (TikTok: `000.mp3`, `type: audio`; Instagram: numbered after the last slide,
  only with `extractor.instagram.audio=true`, and *included in `count`*).
  `separateGalleryAudio` renames it to `audio.<ext>` so it is never a slide:
  not in `files`, not in `file_count`, not in completeness (Instagram's count is
  decremented when the audio was stored). Attribution and status live in
  `metadata.music` (`GalleryMusic`); the Apify builders do the same from
  the record's `music` block (TikTok) or `audio`/`audio_url` (Instagram, which
  the dataset has only ever returned as null). Licensed TikTok tracks arrive as
  M4A whose ISO BMFF brand is `M4A `; the sniffer maps that to `audio/mp4`.
  yt-dlp videos get the same attribution in `VideoMetadata.music` from
  `track`/`artists`/`album` (TikTok fills these; most extractors do not).
- **Bluesky** videos come back as the poster's original atproto blob, so the
  archive is the source file, byte for byte.
- **Facebook** splits by shape. Video permalinks (`/reel/`, `/videos/`,
  `/watch`, `fb.watch`) are yt-dlp items; photo posts and post permalinks
  (`/photo/?fbid=`, `/photo.php?fbid=`, `/<page>/photos/<id>`,
  `/<page>/posts/<id>` including `pfbid…`) are gallery-dl items. The two claims
  are disjoint and pinned that way in the matrix test, so no Facebook URL gets
  two media items. Containers — a page, its photo tab, its feed — stay
  unclaimed ordinary URLs; claiming one would point an extractor at an entire
  account. `story.php` and `permalink.php` are unclaimed too: visibility-gated
  shapes whose dataset coverage is unverified.
- **Login-only sites** (Instagram feed posts, X, Pinterest, Facebook posts) get
  no gallery-dl item without a cookie jar: the run cannot succeed and would
  spend rate limit proving it. The API reports `authentication_required` for
  those. The exception is a site the Apify fallback covers — Instagram,
  X, Pinterest and Facebook posts today — which does get an item when the
  fallback is configured, because the
  guaranteed-failed native run is then followed by one that can actually
  succeed. Routing asks the fallback client itself
  (`utils.SetMediaFallback` carries `Client.SupportsFallback`), so
  coverage lives in one place instead of two lists that can drift.

### Apify fallback

`internal/apify` buys media the free path cannot get. It only ever runs after a
recorded native failure, only for URLs it can plausibly rescue, and it writes a
`FallbackUsage` row per actor run — including failed ones, because a failed
run is still billed for its start and silent spend is the thing that table
exists to prevent. Costs are the platform-reported `usageTotalUsd` of each
run, not estimates. `APIFY_MAX_RUN_COST_USD` only warns; it cannot un-spend.

| Platform | Item type | Actor | How the bytes are obtained |
|---|---|---|---|
| Instagram reel / feed post | yt-dlp, gallery-dl | `data-slayer/instagram-post-details` | direct CDN download; soundtrack from `music_metadata` |
| TikTok video | yt-dlp | `clockworks/tiktok-video-scraper` | actor stores the MP4 in its key-value store (IP-locked CDN) |
| TikTok photo post | gallery-dl | `clockworks/tiktok-video-scraper` | direct CDN download, stored copy per refused still |
| YouTube video | yt-dlp | `epctex/youtube-video-downloader` + `streamers/youtube-scraper` (facts, run concurrently; oEmbed if it fails) | downloader stores the MP4 in its key-value store |
| Facebook video / reel | yt-dlp | `apify/facebook-posts-scraper` | direct `fbcdn` download; a reel that comes back in post shape is retried as `watch?v=` |
| Facebook post | gallery-dl | `apify/facebook-posts-scraper` | direct `scontent` download, crop parameter stripped |
| Reddit post | gallery-dl | `harshmaur/reddit-scraper` | `fallback_url` video + derived `CMAF_AUDIO_128.mp4` muxed with ffmpeg; HLS backstop |
| X status | gallery-dl | `kaitoeasyapi/twitter-x-data-tweet-scraper-pay-per-result-cheapest` | `pbs.twimg.com?name=orig` / best-bitrate `video.twimg.com` MP4; mock rows filtered by ID |
| Pinterest pin | gallery-dl | `silentflow/pinterest-scraper-ppr` | `/originals/` image; progressive 720p MP4, HLS backstop |

Vimeo is deliberately absent: see the Vimeo note under platform routing.

Store-served bytes (TikTok, YouTube) are counted in `BytesTransferred` on the
usage row so provider-side transfer stays visible. The same client also
implements the social-thumbnail contract (`ResolveSocialThumbnail`: poster only,
no media download, no store copies; YouTube posters come free from `i.ytimg.com`)
and the stored-video metadata refresh (`RefreshStoredVideoMetadata`: record
only, historical media facts preserved).

Raw provider records are sanitized before storage, in the sidecar and inside
the gallery ZIP: on a signed media host every query parameter is redacted,
because the credential-bearing parameter names are provider-specific
(`s`/`e`/`v` on redd.it, `policy`/`signature`/`tk` on TikTok's CDNs) and
guessing which one is the secret is how one gets left behind. Fixtures under
`internal/apify/testdata` are real records with signed values replaced by
`SYNTHETIC-NOT-A-REAL-SECRET`; the tests assert that marker never reaches an
artifact.

Archives rescued before the swap carry `source = "brightdata"` and a
`brightdata.json` record; readers keep honoring both (`models.IsFallbackSource`).

### Database Changes
1. Update models in `internal/models/models.go`
2. Add migration logic to `cmd/main.go` (AutoMigrate call)
3. Test with `make db-reset` for clean database

## Troubleshooting

### Common Issues
- **Playwright fails**: Ensure Chromium is installed (`playwright install chromium`)
- **yt-dlp not found**: Install with `pip install yt-dlp`
- **gallery-dl not found**: Install with `pip install gallery-dl`. Install it into the *same* Python environment as yt-dlp: gallery-dl hands DASH manifests and reddit videos to yt-dlp as an importable module. Instagram silently falls back to a lower-quality pre-merged MP4 without it; reddit videos fail outright, logging `Cannot import yt-dlp or youtube-dl`.
- **Reddit videos archive without sound**: check ffmpeg is installed (yt-dlp merges the separate DASH audio stream with it) and that gallery-dl can `import yt_dlp` from its own interpreter.
- **Database connection**: Check PostgreSQL is running and credentials match
- **Permission errors**: Ensure storage/cache directories are writable
- **Browser leaks**: Check `/status/browser` endpoint for monitoring data

### Production Debugging
- **SSH Access**: `ssh archive-hq-local.selfhosted.hackclub.com` (via cloudflared, see `~/.ssh/config`; use `sudo docker ...` on the host)
- **Health Checks**: Monitor `/health` and `/status/browser` endpoints
- **Logs**: Check Coolify dashboard or container logs
- **Database**: Connect via environment variables in deployment
- **Container names rotate per deploy** — resolve the app container with
  `sudo docker ps | grep ^hcmolbh8` rather than assuming a fixed name
- **Don't run one-off tools inside the app container**: a Coolify deploy will
  kill it mid-run. Run them on the host instead.

### Health Monitoring
- Startup health checks verify yt-dlp, gallery-dl, and Playwright availability
- Browser process monitoring with leak detection
- Automatic log cleanup (30 days for completed items)

## Architecture Notes

- **Modular Design**: Clean separation of concerns with internal packages
- **Interface-Driven**: Storage and Archiver interfaces for extensibility
- **Resilient Processing**: Error handling with retries, timeouts, and status tracking
- **Memory Efficient**: Streaming operations for large files
- **Production Ready**: Docker deployment with health checks and resource limits
- **Git Integration**: Full Git HTTP backend for repository cloning
- **API-First**: RESTful API with web interface as overlay
- **Queue Management**: Robust job queue with worker pool and retry logic
- **Browser Safety**: Process monitoring and cleanup to prevent resource leaks
