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
- **Python 3 + gallery-dl** (for photo posts and mixed photo/video carousels)
- **Python 3 + itch-dl** (for itch.io game archiving)
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
│   │   ├── gallery_dl_serve.go # gallery-dl manifest + per-file serving
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
│   │   └── thumbnail.go    # Crop/scale/encode helper
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
- `GET /api/v1/past-archives?url=...` - Get past archives for URL

### Public Access
- `GET /:shortid` - Archive display page with tabs for each type
- `GET /archive/:shortid/:type` - Download specific archive type
- `GET /archive/:shortid/mhtml/html` - View MHTML as rendered HTML
- `GET /git/:shortid` - Git HTTP backend for cloning repositories
- `GET /itch/:shortid/file/*filepath` - Stream individual files from itch.io game archives
- `GET /itch/:shortid/list` - JSON list of files in itch.io game archive
- `GET /gallery/:shortid/list` - JSON post metadata + media file list for a gallery-dl archive
- `GET /gallery/:shortid/file/*filepath` - Stream one media file out of a gallery-dl archive
- `GET|HEAD /thumb/:shortid` - Preview image for a capture (480x270 JPEG); falls back to an SVG placeholder and queues generation
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
- `GET /health` - Application health check
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
four columns on `archive_items` (`thumbnail_key/width/height/status`) and are
served from `/thumb/{shortid}[/{type}]`.

- **Size/format**: 480x270 JPEG (`internal/thumbnail`). JPEG because the only
  WebP encoder in the tree (`nativewebp`) is lossless-only — it produced a
  ~490KB "thumbnail" in testing versus ~35KB for JPEG, and it panics on some
  low-colour-count inputs. `x/image/webp` is decode-only and reads back what
  `nativewebp` wrote.
- **Each archiver supplies its own source**, inline, from work it already does:
  | Type | Source | Crop |
  | --- | --- | --- |
  | `screenshot` | the full-page image it already decoded — no extra browser work | `CropTop` |
  | `yt-dlp` | the platform's own poster via `--write-thumbnail` (YouTube serves **WebP**) | `CropCenter` |
  | `gallery-dl` | the post's first still image, from the temp dir before it is zipped | `CropCenter` |
  | `mhtml`, `git`, `itch` | none — the capture falls back to a sibling item's thumbnail | — |
- **The crop anchor is a required argument, and it matters.** A page screenshot
  is `CropTop` (its identity is the header). A video or photo thumbnail is
  `CropCenter` — a 9:16 reel cover frames its subject in the middle, and
  top-cropping returns the empty space above their head.
- **Only `screenshot` can be backfilled** (`CanDeriveFromArchive`). The `/thumb`
  handler enqueues a `ThumbnailJobArgs` job on the `high_priority` queue the
  first time somebody views an archive lacking one. Video and gallery posters
  exist only at capture time, so pre-existing ones stay without a thumbnail by
  design.
- **Never generate inline in a request** — a full-page screenshot reaches 60
  megapixels (~240MB decoded) and one dashboard render asks for hundreds.
- **gallery-dl's thumbnail must be built before the ZIP goroutine starts.** That
  goroutine owns `cleanupTmp`, so the downloaded files can vanish once it runs.
- **`thumbnail_status` must be set to `unavailable` for permanent failures**
  (unsupported type, oversized, undecodable). Without it the lazy path
  re-enqueues the same impossible job on every page view.
- **A thumbnail failure must never fail an archive.** The archive is the
  product; the preview is not.
- Keys carry an upload nonce like archive keys (`{shortid}/{type}-{nonce}-thumb.jpg`)
  because the bucket forbids overwrites and deletes. Regenerating means writing a
  new object and repointing the row.
- `/thumb` always returns an image, falling back to a generated SVG placeholder
  with a short `max-age` so a refresh picks up the real one. Callers can render
  a card unconditionally.

### Database Changes
1. Update models in `internal/models/models.go`
2. Add migration logic to `cmd/main.go` (AutoMigrate call)
3. Test with `make db-reset` for clean database

## Troubleshooting

### Common Issues
- **Playwright fails**: Ensure Chromium is installed (`playwright install chromium`)
- **yt-dlp not found**: Install with `pip install yt-dlp`
- **gallery-dl not found**: Install with `pip install gallery-dl`. Install it into the *same* Python environment as yt-dlp: gallery-dl's default Instagram video path hands DASH manifests to yt-dlp as an importable module, and silently falls back to lower-quality pre-merged MP4 otherwise.
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
