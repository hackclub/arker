# Arker Development Guide

Arker is a Go-based web archiving server that captures web pages using multiple strategies and provides short URLs for accessing archived content.

## Deployment

**Production URL**: https://archive.selfhosted.hackclub.com  
**Deployment**: Managed via Coolify  
**Debug Access**: `ssh root@archive.selfhosted.hackclub.com`

## Quick Commands

### Essential Development Commands
- **Build**: `go build -o arker ./cmd`
- **Test**: `go test -v`
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
2. Install Playwright: `go install github.com/playwright-community/playwright-go/cmd/playwright@latest && playwright install chromium`
3. Install yt-dlp: `pip install yt-dlp`
4. Run: `go run .`

### Dependencies
- **Go 1.24+** (using Go 1.24.5 toolchain)
- **PostgreSQL 15**
- **Git** (for repository archiving)
- **Python 3 + yt-dlp** (for YouTube archiving)
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
│   │   ├── youtube.go      # YouTube video downloading
│   │   ├── itch.go         # itch.io game archiving
│   │   └── browser_utils.go # Shared browser utilities
│   ├── handlers/           # HTTP handlers
│   │   ├── admin.go        # Admin interface endpoints
│   │   ├── api.go          # REST API endpoints
│   │   ├── auth.go         # Authentication handlers
│   │   ├── display.go      # Archive display pages
│   │   ├── git.go          # Git HTTP backend
│   │   ├── itch_serve.go   # itch.io individual file serving
│   │   └── serve.go        # File serving with streaming
│   ├── models/             # Database models & types
│   │   └── models.go       # User, ArchivedURL, Capture, ArchiveItem
│   ├── storage/            # Storage interface & implementations
│   │   └── fs.go           # Filesystem storage (S3-ready interface)
│   ├── monitoring/         # Browser process monitoring
│   ├── utils/              # Shared utilities
│   └── workers/            # Async job processing
│       ├── queue.go        # Job queue management
│       └── worker.go       # Background worker implementation
├── templates/              # HTML templates for web interface
└── Makefile               # Development workflow commands
```

## Core Interfaces & Architecture

### Key Interfaces
- **`Storage`** - Pluggable storage backend (filesystem now, S3-ready)
  - Methods: `Writer(key)`, `Reader(key)`, `Exists(key)`, `Size(key)`
  - Current: Filesystem storage with zstd compression
- **`Archiver`** - Different archiving strategies
  - Methods: `Archive(url, writer)`, content type detection
  - Types: MHTML, Screenshot, Git, YouTube, Itch

### Performance Features
- **Browser Instance Reuse**: Playwright browsers reused across jobs for efficiency
- **Streaming**: All file operations use streaming with zstd compression
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
git clone https://archive.selfhosted.hackclub.com/git/{shortid}
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
- `storage_test.go` - Storage interface tests
- `archiver_test.go` - Archiver interface tests
- `monitoring_test.go` - Browser monitoring tests
- `validation_test.go` - Input validation tests
- `login_text_test.go` - Login text handling tests

- `vimeo_test.go` - Vimeo video archiving tests

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
- **playwright-community/playwright-go** v0.4501.1 - Browser automation
- **go-git/go-git/v5** v5.8.1 - Git operations
- **klauspost/compress** v1.18.0 - zstd compression

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
2. Add to `archiversMap` in `cmd/main.go`
3. Update content type detection in handlers
4. Add tests in `archiver_test.go`

### Database Changes
1. Update models in `internal/models/models.go`
2. Add migration logic to `cmd/main.go` (AutoMigrate call)
3. Test with `make db-reset` for clean database

## Troubleshooting

### Common Issues
- **Playwright fails**: Ensure Chromium is installed (`playwright install chromium`)
- **yt-dlp not found**: Install with `pip install yt-dlp`
- **Database connection**: Check PostgreSQL is running and credentials match
- **Permission errors**: Ensure storage/cache directories are writable
- **Browser leaks**: Check `/status/browser` endpoint for monitoring data

### Production Debugging
- **SSH Access**: `ssh root@archive.selfhosted.hackclub.com`
- **Health Checks**: Monitor `/health` and `/status/browser` endpoints
- **Logs**: Check Coolify dashboard or container logs
- **Database**: Connect via environment variables in deployment

### Health Monitoring
- Startup health checks verify yt-dlp and Playwright availability
- Browser process monitoring with leak detection

- Automatic log cleanup (30 days for completed items)

## Architecture Notes

- **Modular Design**: Clean separation of concerns with internal packages
- **Interface-Driven**: Storage and Archiver interfaces for extensibility
- **Resilient Processing**: Error handling with retries, timeouts, and status tracking
- **Memory Efficient**: Streaming operations for large files with compression
- **Production Ready**: Docker deployment with health checks and resource limits
- **Git Integration**: Full Git HTTP backend for repository cloning
- **API-First**: RESTful API with web interface as overlay
- **Queue Management**: Robust job queue with worker pool and retry logic
- **Browser Safety**: Process monitoring and cleanup to prevent resource leaks
