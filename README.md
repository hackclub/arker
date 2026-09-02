# Arker

A self-hostable minimalist version of <https://archive.org>.

- Creates Chrome snapshots of URLs and serves them at nice short URLs like <https://archive.hackclub.com/p9OGi>
- Also supports git clones, videos (yt-dlp), photo posts and carousels (gallery-dl), itch.io games, and website screenshots
- Comprehensive API

The API-key-protected `POST /api/v1/archive` returns the original public `url`
plus `short_id` and `result_url`. Poll `GET /api/v1/archive/:shortid` for the
schema-versioned unified result: all capture items, normalized social metadata,
Arker-stored media URLs, provenance, sanitized provider-metadata links, and a
USD cost summary. Native operations are surfaced as free; Bright Data usage
includes successful and failed billable attempts and is marked as estimated.
Known captures return 200 even while pending or after a partial/failed result;
unknown IDs return 404. Capture aliases expose both requested and canonical IDs.

- Stores everything compressed using [zstd](https://github.com/facebook/zstd) level 6 (seekable format for random access)
- Flexible storage: local filesystem or S3-compatible cloud storage

Try out the demo instance at <https://arker-demo.hackclub.com>.

## Configuration

### Using .env Files

Arker supports loading configuration from a `.env` file for easier local development and deployment. 

1. **Copy the example file:**
   ```bash
   cp .env.example .env
   ```

2. **Edit the `.env` file** with your specific configuration values

3. **Run the server** - it will automatically load the `.env` file if present

The `.env` file is optional. If it doesn't exist, Arker will use environment variables or default values. Environment variables always take precedence over `.env` file values.

### Environment Variables

- `DB_URL` - PostgreSQL connection string (default: `host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable`)
- `STORAGE_PATH` - Archive storage directory (default: `./storage`) - *only used when `STORAGE_TYPE=filesystem`*
- `CACHE_PATH` - Git clone cache directory (default: `./cache`)
- `MAX_WORKERS` - Worker pool size (default: `5`)
- `PORT` - HTTP server port (default: `8080`)
- `SESSION_SECRET` - Session encryption key (auto-generated if not set)
- `ADMIN_USERNAME` - Admin login username (default: `admin`)
- `ADMIN_PASSWORD` - Admin login password (default: `admin`)
- `LOGIN_TEXT` - Custom text to display under the login form. Useful for providing demo credentials (e.g., `LOGIN_TEXT="Demo: admin/admin"`). Supports basic HTML.
- `GIN_MODE` - Gin framework mode (`debug` for development)
- `YTDLP_COOKIES_FILE` / `YTDLP_COOKIES_B64` - Optional Netscape cookies.txt, shared by yt-dlp and gallery-dl. Required for Instagram and other sites that require login.
- `YTDLP_PROXY` - Optional proxy URL passed to yt-dlp and gallery-dl. A residential/mobile proxy may be needed when Instagram rate-limits datacenter IPs. `socks5://` needs PySocks (`pip install "requests[socks]"`, already in the Docker images).
- `YTDLP_IMPERSONATE` - Optional yt-dlp `--impersonate` target for Instagram/TikTok/Facebook video URLs. Production/dev Docker images default this to `chrome` and install `curl-cffi`; set it empty to disable.

### Itch.io Game Archiving

- `ITCH_API_KEY` - itch.io API key for downloading games (required for itch.io archiving)
- `ITCH_DL_PATH` - Path to itch-dl command (default: `itch-dl`)

**Dependencies for itch.io support:**
- Python 3.10+ with `itch-dl` package: `pip install itch-dl`
- itch.io API key: Generate at https://itch.io/user/settings → API Keys

### Video Archiving (yt-dlp)

Arker shells out to `yt-dlp` for YouTube/Vimeo/Instagram-reel/TikTok/Facebook-style videos. The production Dockerfile installs yt-dlp from the nightly (`--pre`) channel with `curl-cffi` because Instagram extractor fixes often land before stable releases. The Docker build also cache-busts on the latest yt-dlp nightly release metadata so redeploys do not keep a stale yt-dlp layer.

Each new video capture stores three durable objects: the playable/remuxed media,
a normalized provider-neutral post record, and the sanitized raw yt-dlp or
Bright Data provider record. `GET /video/:shortid/manifest` returns the capture
status, the existing `/archive/:shortid/yt-dlp` media URL, and normalized
metadata. `GET /video/:shortid/raw` returns the sanitized raw record for audits.
Older completed videos remain playable; their manifest explicitly returns
`metadata_available: false` rather than guessing metadata from logs or the URL.

Re-archiving a URL whose video is already stored never downloads the media
again: the new capture shares the earlier media object and runs yt-dlp with
`--skip-download` to write fresh metadata, captions, and poster sidecars under
its own capture (MHTML and screenshot re-run as normal). If even the metadata
refresh fails — a deleted post, a platform refusal — the new capture inherits
the earlier sidecars instead of failing, since the archive already holds the
product.

`GET /thumb/:shortid` is a stable, public, non-redirecting image URL. Social
previews preserve the post's full aspect ratio while fitting within 480px;
ordinary web pages use a compact 480x270 preview derived from the page
screenshot. The same URL serves a placeholder before the preview is ready and
the real image afterward; nonexistent captures return 404.

Administrators can repair historical social previews with
`POST /admin/backfill-social-thumbnails?cost_limit_usd=5`. The resumable,
one-worker queue extracts stills from stored gallery ZIPs, refreshes video
posters without downloading videos, groups duplicate canonical URLs, and never
spends past the shared provider cap. `GET` on the same endpoint reports item
and queue progress; pass `?since=<RFC3339>` to include spend for that run.

Historical video sidecars can be repaired independently with
`POST /admin/backfill-video-metadata` (or previewed with `?dry_run=true`). Its
two-worker queue makes one bounded, media-disabled yt-dlp request, probes the
already-stored video for intrinsic dimensions/duration, and shares fresh
normalized/raw sidecars across duplicate canonical URLs. On the final attempt,
covered platform failures use a metadata-only Bright Data lookup; it buys the
post record (or a YouTube page resolution) but never downloads or replaces the
stored video. `GET` on the same endpoint reports coverage and queue progress.

For manual installs, prefer:

```bash
pip3 install --upgrade --pre "yt-dlp[default,curl-cffi]"
YTDLP_IMPERSONATE=chrome  # used for Instagram/TikTok/Facebook video URLs
```

### Photo and Carousel Archiving (gallery-dl)

yt-dlp only downloads video. A URL whose media is photos — an Instagram feed
post, an X status, a Reddit gallery — makes it fail outright with *"There is no
video in this post"*. Those URLs go to `gallery-dl` instead, which fetches every
image and video in the post along with the caption, author, date, and like count.

The result is a ZIP holding every downloaded file, gallery-dl's raw per-file
metadata sidecars, and a normalized `metadata.json` written by Arker.

`GET /gallery/:shortid/manifest` is the counterpart of the video manifest and
the endpoint API consumers should use: capture status, normalized post
metadata, and one complete `media_url` per card in swipe order, each fetchable
on its own from `/gallery/:shortid/file/<name>`. Nobody has to download the ZIP
to read a carousel, and no caller has to build a path out of the capture tool's
name. Unfinished, failed and legacy captures answer `200` with the state named
explicitly. `/gallery/:shortid/raw` returns sanitized provider sidecars, the
viewer's own `/gallery/:shortid/list` is unchanged, and the ZIP remains
available at `/archive/:shortid/gallery-dl`.

Routed hosts (post-shaped URLs only, so a profile link never pulls a whole
account): Instagram `/p/` and `/tv/`, X/Twitter, Reddit, Tumblr, Bluesky, Flickr,
Imgur, DeviantArt, ArtStation, Pixiv, Pinterest, Newgrounds, VSCO. Adding one is
a single entry in `galleryDLSites` in `internal/utils/url_utils.go`.

```bash
pip3 install --upgrade gallery-dl "requests[socks]"
```

Install gallery-dl into the **same** Python environment as yt-dlp: gallery-dl's
default Instagram video path hands DASH manifests to yt-dlp as an importable
module, and silently falls back to lower-quality pre-merged MP4 if it cannot
import one.

Instagram archiving needs cookies (`YTDLP_COOKIES_FILE`) — logged out, every
request redirects to the login page. Do not add a `GALLERYDL_SLEEP_REQUEST`
override to "be safe": gallery-dl already waits a randomized 6-12 seconds between
Instagram API calls, and that setting replaces the per-site default rather than
acting as a floor.

### Bright Data Fallback (Instagram & YouTube)

The native yt-dlp/gallery-dl flows fail in ways Arker cannot fix from its own
network position: Instagram login walls and account throttles, YouTube
geo-blocks and bot checks. With a Bright Data API key configured, a failed
native run on an Instagram or YouTube URL gets one paid second chance. The
native flows always run first and their successes are always preferred — the
fallback spends money only after a native failure.

- **Instagram** uses Bright Data's Web Scraper API (the maintained
  Instagram-specific scrapers): one dataset trigger returns post metadata plus
  direct CDN media URLs, and the media itself downloads over Arker's own
  connection for free. Reels become the usual `.mp4` artifact; feed posts
  become the usual gallery ZIP with `metadata.json` (plus the raw Bright Data
  record as `brightdata.json`), so viewers and the API see no difference.
- **YouTube** cannot be served by the scraper alone: its `video_url` is signed
  for Bright Data's own exit IP (deliberately scrambled `ip=` parameter), and
  their proxy products refuse YouTube. Instead the fallback opens a Bright
  Data **Browser API** session and resolves the video via YouTube's Innertube
  player API from inside that session — no scraping of the rendered page —
  then pulls the progressive MP4 through the same session. Progressive streams
  top out at 360p/720p; provenance is recorded on the archive item
  (`source = "brightdata"`) so fidelity can be audited later.

Every Bright Data operation writes a row to the `bright_data_usages` table with
an estimated cost; `GET /admin/brightdata-usage` (admin session) reports totals,
per-product and per-day spend, and recent events. Costs are computed from the
configured rates below — the Bright Data dashboard remains the invoice of
record.

```bash
BRIGHTDATA_API_KEY=...                      # required to enable the fallback
BRIGHTDATA_CUSTOMER_ID=...                  # optional; resolved via the API at startup
BRIGHTDATA_BROWSER_ZONE=mcp_browser_no_ratelimit  # Browser API zone for YouTube
BRIGHTDATA_BROWSER_ZONE_PASSWORD=...        # optional; resolved via the API at startup
BRIGHTDATA_SCRAPER_COST_PER_RECORD=0.0015   # USD per Web Scraper API record
BRIGHTDATA_BROWSER_COST_PER_GB=8.40         # USD per GB of Browser API traffic
BRIGHTDATA_YT_CLIENT_NAME=ANDROID           # Innertube client for YouTube resolution
BRIGHTDATA_YT_CLIENT_VERSION=20.10.38       # bump via env if YouTube retires it
```

With the fallback enabled, Instagram gallery items are created even when no
cookie jar is configured: the native run still fails fast, but the item now has
a real path to success instead of being skipped outright.

### Storage Configuration

Arker supports both filesystem and S3-compatible storage backends.

#### Filesystem Storage (Default)
```bash
STORAGE_TYPE=filesystem  # or omit (default)
STORAGE_PATH=./storage
```

#### S3-Compatible Storage
```bash
STORAGE_TYPE=s3
S3_BUCKET=your-bucket-name        # Required
S3_REGION=us-east-1              # Default: us-east-1
S3_ACCESS_KEY_ID=your-key-id     # Optional: uses AWS credential chain if omitted
S3_SECRET_ACCESS_KEY=your-secret # Optional: uses AWS credential chain if omitted
S3_ENDPOINT=https://s3.example.com  # Optional: for non-AWS S3-compatible services
S3_PREFIX=arker/                 # Optional: prefix for all keys
S3_FORCE_PATH_STYLE=true         # Required for MinIO and some providers
S3_PUBLIC_BASE_URL=https://cdn.example.com  # Optional: public bucket/CDN base URL for direct downloads
S3_DIRECT_URL_EXPIRATION=12h      # Optional: presigned GET URL lifetime when no public base URL is set
```

**Supported S3-Compatible Services:**
- AWS S3
- MinIO
- Backblaze B2
- DigitalOcean Spaces
- Google Cloud Storage (S3 API)
- Any S3-compatible storage service

**Example Configurations:**

AWS S3:
```bash
STORAGE_TYPE=s3
S3_BUCKET=my-arker-archives
S3_REGION=us-west-2
```

MinIO:
```bash
STORAGE_TYPE=s3
S3_ENDPOINT=https://minio.example.com
S3_BUCKET=arker
S3_ACCESS_KEY_ID=minioadmin
S3_SECRET_ACCESS_KEY=minioadmin
S3_FORCE_PATH_STYLE=true
```

Backblaze B2:
```bash
STORAGE_TYPE=s3
S3_ENDPOINT=https://s3.us-west-002.backblazeb2.com
S3_BUCKET=my-arker-bucket
S3_REGION=us-west-002
S3_ACCESS_KEY_ID=your-b2-key-id
S3_SECRET_ACCESS_KEY=your-b2-secret
```

## Deployment Notes

### Docker Deployment

The Dockerfile includes all necessary dependencies including `itch-dl`. For production deployment:

1. **Set Environment Variables:**
   ```bash
   ITCH_API_KEY=your_itch_api_key_here  # Required for itch.io archiving
   ```

2. **Build and Deploy:**
   ```bash
   docker build -t arker .
   docker run -e ITCH_API_KEY="your_key" -p 8080:8080 arker
   ```

### Manual Installation

If not using Docker, install the Python dependencies manually:

```bash
# Install itch-dl for itch.io game archiving
pip3 install itch-dl

# Verify installation
python3 -m itch_dl --help
```

## License

MIT
