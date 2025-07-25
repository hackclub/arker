# Arker Development Guide

## Build and Test Commands

- **Build**: `go build -o arker ./cmd`
- **Test**: `go test -v`
- **Run locally**: `go run ./cmd`
- **Docker build**: `docker compose build`
- **Docker run**: `docker compose up`

## Environment Setup

### Local Development
1. Start PostgreSQL: `docker run -d --name postgres -e POSTGRES_USER=user -e POSTGRES_PASSWORD=pass -e POSTGRES_DB=arker -p 5432:5432 postgres:15`
2. Install Playwright: `go install github.com/playwright-community/playwright-go/cmd/playwright@latest && playwright install chromium`
3. Install yt-dlp: `pip install yt-dlp`
4. Run: `go run .`

### Docker Development
1. `docker compose up --build`

## Project Structure

- `cmd/main.go` - Main application entry point
- `internal/` - Internal packages (modular architecture)
  - `archivers/` - Archive implementations (MHTML, screenshot, git, youtube)
  - `handlers/` - HTTP handlers for API and web interface
  - `models/` - Database models and types
  - `storage/` - Storage interface and implementations
  - `utils/` - Shared utilities (logging, health checks, timeouts)
  - `workers/` - Job processing and queue management
- `templates/` - HTML templates for web interface
- `*_test.go` - Tests for storage and archiver interfaces
- `Dockerfile` - Container build configuration
- `docker-compose.yml` - Multi-service orchestration

## Key Interfaces

- `Storage` - Pluggable storage backend (filesystem now, S3 ready)
- `Archiver` - Different archiving strategies (MHTML, Git, YouTube, Screenshot)

## Default Credentials

- Username: `admin`
- Password: `admin`

## API Endpoints

- `POST /api/v1/archive` - Request new archive
- `GET /:shortid` - View archive display page  
- `GET /archive/:shortid/:type` - Download specific archive type
- `GET /git/:shortid` - Git HTTP backend for cloning

## Configuration

Environment variables:
- `DB_URL` - PostgreSQL connection string
- `STORAGE_PATH` - Archive storage path (default: ./storage)
- `CACHE_PATH` - Git clone cache path (default: ./cache)
- `MAX_WORKERS` - Worker pool size (default: 5)

## Architecture Notes

- **Modular Design**: Clean separation of concerns with internal packages
- **Performance Optimized**: Reuses Chromium pages for MHTML+screenshot jobs (2x speedup)
- **Resilient**: Error handling with retries, timeouts, and health checks
- **Streaming**: Uses streaming compression with zstd for all file operations
- **Async Processing**: Queue-based processing with configurable workers
- **Git Support**: HTTP cloning via git-http-backend
- **Storage Abstraction**: Modular storage interface for easy S3 migration
- **Authentication**: Session-based admin authentication
