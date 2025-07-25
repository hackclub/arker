# Arker Development Guide

Arker is a Go-based web archiving server that captures web pages using multiple strategies (MHTML, screenshots, Git repos, YouTube videos) and provides short URLs for accessing archived content.

## Quick Commands

### Essential Development Commands
- **Build**: `go build -o arker ./cmd`
- **Test**: `go test -v` (tests storage and archiver interfaces)
- **Run locally**: `go run ./cmd` or `go run .`
- **Lint/Format**: `go fmt ./...` and `go vet ./...`
- **Build check**: `go build ./...` (compile check without executable)

### Docker Development
- **Start dev environment**: `make dev` (uses docker-compose.dev.yml with live reload)
- **Build dev containers**: `make dev-build`
- **Stop dev environment**: `make dev-down`
- **View dev logs**: `make dev-logs`
- **Clean all containers**: `make clean`

### Database Operations
- **Connect to dev DB**: `make db-connect`
- **Reset dev database**: `make db-reset`
- **Manual PostgreSQL**: `docker run -d --name postgres -e POSTGRES_USER=user -e POSTGRES_PASSWORD=pass -e POSTGRES_DB=arker -p 5432:5432 postgres:15`

### Production
- **Start production**: `make prod` or `docker compose up -d`
- **Stop production**: `make prod-down`
- **View production logs**: `make prod-logs`

## Environment Setup

### Local Development (Option 1)
1. Start PostgreSQL: `docker run -d --name postgres -e POSTGRES_USER=user -e POSTGRES_PASSWORD=pass -e POSTGRES_DB=arker -p 5432:5432 postgres:15`
2. Install Playwright: `go install github.com/playwright-community/playwright-go/cmd/playwright@latest && playwright install chromium`
3. Install yt-dlp: `pip install yt-dlp`
4. Run: `go run .`

### Docker Development (Option 2 - Recommended)
1. `make dev` (starts PostgreSQL + app with live reload)

### Dependencies
- **Go 1.22+** (using Go 1.24.3 toolchain)
- **PostgreSQL 15**
- **Git** (for repository archiving)
- **Python 3 + yt-dlp** (for YouTube archiving)
- **Playwright + Chromium** (for MHTML and screenshots)

## Project Structure

```
/
├── cmd/main.go              # Application entry point & service setup
├── internal/                # Internal packages (modular architecture)
│   ├── archivers/          # Archive implementations
│   │   ├── mhtml.go        # MHTML webpage archiving
│   │   ├── screenshot.go   # Full-page screenshot capture
│   │   ├── git.go          # Git repository cloning
│   │   └── youtube.go      # YouTube video downloading
│   ├── handlers/           # HTTP handlers
│   │   ├── admin.go        # Admin interface endpoints
│   │   ├── api.go          # REST API endpoints
│   │   ├── auth.go         # Authentication handlers
│   │   ├── display.go      # Archive display pages
│   │   ├── git.go          # Git HTTP backend
│   │   └── serve.go        # File serving with streaming
│   ├── models/             # Database models & types
│   │   └── models.go       # User, ArchivedURL, Capture, ArchiveItem
│   ├── storage/            # Storage interface & implementations
│   │   └── fs.go           # Filesystem storage (S3-ready interface)
│   ├── utils/              # Shared utilities
│   │   ├── health.go       # Health checks (yt-dlp, Playwright, etc.)
│   │   ├── logging.go      # Structured logging
│   │   └── timeout.go      # Request timeout utilities
│   └── workers/            # Async job processing
│       ├── queue.go        # Job queue management
│       └── worker.go       # Background worker implementation
├── templates/              # HTML templates for web interface
├── storage_test.go         # Storage interface tests
├── archiver_test.go        # Archiver interface tests
├── Dockerfile              # Production container
├── Dockerfile.dev          # Development container with live reload
├── docker-compose.yml      # Production multi-service setup
├── docker-compose.dev.yml  # Development setup with volume mounts
└── Makefile               # Development workflow commands
```

## Core Interfaces & Architecture

### Key Interfaces
- **`Storage`** - Pluggable storage backend (filesystem now, S3-ready)
  - Methods: `Writer(key)`, `Reader(key)`, `Exists(key)`
  - Current: Filesystem storage with zstd compression
- **`Archiver`** - Different archiving strategies
  - Methods: `Archive(url, writer)`, content type detection
  - Types: MHTML, Screenshot, Git, YouTube

### Performance Features
- **Page Reuse**: Chromium browser pages are reused for MHTML+screenshot jobs (2x speedup)
- **Streaming**: All file operations use streaming with zstd compression
- **Async Processing**: Queue-based job processing with configurable worker pools
- **Concurrent Workers**: Default 5 workers (configurable via `MAX_WORKERS`)

### Database Models
- **User**: Admin authentication (default: admin/admin)
- **ArchivedURL**: Original URLs with metadata
- **Capture**: Archive sessions with short IDs (8-char alphanumeric)
- **ArchiveItem**: Individual archive files per type with logs & status

## API Endpoints

### Public API
- `POST /api/v1/archive` - Request new archive
  ```json
  {"url": "https://example.com", "types": ["mhtml", "screenshot"]}
  ```
- `GET /api/v1/past-archives?url=...` - Get past archives for URL
- `GET /:shortid` - Archive display page with tabs for each type
- `GET /archive/:shortid/:type` - Download specific archive type
- `GET /archive/:shortid/mhtml/html` - View MHTML as rendered HTML
- `GET /git/:shortid` - Git HTTP backend for cloning repositories

### Admin Interface
- `GET /login` - Admin login page
- `POST /login` - Authentication endpoint
- `GET /` - Admin dashboard with archive management
- `POST /admin/url/:id/capture` - Request new capture
- `GET /admin/item/:id/log` - View capture logs
- `GET /logs/:shortid/:type` - View processing logs

### Git Repository Access
```bash
git clone http://localhost:8080/git/{shortid}
```

## Configuration

### Environment Variables
- `DB_URL` - PostgreSQL connection string (default: local PostgreSQL)
- `STORAGE_PATH` - Archive storage directory (default: `./storage`)
- `CACHE_PATH` - Git clone cache directory (default: `./cache`)
- `MAX_WORKERS` - Worker pool size (default: `5`)
- `GIN_MODE` - Gin framework mode (`debug` for development)

### Default Credentials
- **Username**: `admin`
- **Password**: `admin`
- **⚠️ Change in production!**

### Security Notes
- Update session secret in production (currently: "secret-key-change-in-production")
- Configure proper PostgreSQL credentials
- Consider rate limiting for API endpoints

## Testing

### Test Files
- `storage_test.go` - Tests storage interface (filesystem operations, tar creation)
- `archiver_test.go` - Tests archiver interfaces (structure validation, content type detection)

### Test Categories
- **Storage Interface**: File operations, compression, existence checks
- **Archiver Structure**: Interface compliance for all archiver types
- **Tar Operations**: Directory archiving for Git repositories
- **Content Type Detection**: MIME type mapping for different archive types

### Running Tests
```bash
go test -v                    # Run all tests
go test -v ./internal/...     # Test internal packages
go test -run TestFSStorage    # Run specific test
```

## Key Dependencies

### Core Framework
- **gin-gonic/gin** v1.9.1 - HTTP router and middleware
- **gorm.io/gorm** v1.25.2 - ORM with PostgreSQL driver
- **gin-contrib/sessions** v0.0.5 - Session management

### Archive & Browser
- **playwright-community/playwright-go** v0.4501.1 - Browser automation
- **go-git/go-git/v5** v5.8.1 - Git operations
- **klauspost/compress** v1.16.7 - zstd compression

### Utilities
- **golang.org/x/crypto** v0.23.0 - Password hashing (bcrypt)
- **golang.org/x/net** v0.25.0 - Network utilities

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

### Adding New API Endpoints
1. Add handler in appropriate `internal/handlers/` file
2. Register route in `cmd/main.go`
3. Update this documentation

## Troubleshooting

### Common Issues
- **Playwright fails**: Ensure Chromium is installed (`playwright install chromium`)
- **yt-dlp not found**: Install with `pip install yt-dlp`
- **Database connection**: Check PostgreSQL is running and credentials match
- **Permission errors**: Ensure storage/cache directories are writable
- **Memory issues**: Increase Docker memory limits (default: 2GB for Playwright)

### Health Checks
- Startup health checks verify yt-dlp and Playwright availability
- Non-critical failures log warnings but don't stop startup
- Manual health check: Check application logs on startup

### Log Cleanup
- Archive logs are automatically cleaned after 30 days for completed items
- Runs daily cleanup routine in background
- Logs available via admin interface and API endpoints

## Architecture Notes

- **Modular Design**: Clean separation of concerns with internal packages
- **Interface-Driven**: Storage and Archiver interfaces for extensibility
- **Resilient Processing**: Error handling with retries, timeouts, and status tracking
- **Memory Efficient**: Streaming operations for large files
- **Production Ready**: Docker deployment with health checks and resource limits
- **Git Integration**: Full Git HTTP backend for repository cloning
- **Session Security**: Secure cookie-based authentication
- **Queue Management**: Robust job queue with worker pool and retry logic
