# Arker Development Guide

## Build and Test Commands

- **Build**: `go build -o arker .`
- **Test**: `go test -v`
- **Run locally**: `go run .`
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

- `main.go` - Main application with all core functionality
- `templates/` - HTML templates for web interface
- `storage_test.go` - Tests for storage and utility functions
- `archiver_test.go` - Tests for archiver interfaces
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

- Uses streaming compression with zstd for all file operations
- Queue-based async processing with configurable workers
- Git repositories support HTTP cloning via git-http-backend
- Modular storage interface for easy S3 migration
- Session-based admin authentication
