# Arker - Web Archiving Server

Arker is a web archiving server written in Go that captures web pages using multiple strategies and provides a short URL interface for accessing archived content.

## Features

- **Multiple Archive Types**:
  - MHTML (complete webpage with resources)
  - Full-page screenshots
  - Git repository cloning
  - YouTube video downloads via yt-dlp

- **Admin Interface**: Web-based admin panel for managing archived URLs and viewing capture history

- **Short URLs**: Clean, short URLs for accessing archived content (e.g., `arker.hackclub.com/hc139d`)

- **Display Page**: Archive viewer with tabs for each archive type and metadata bar

- **Git Clone Support**: Archived git repositories can be cloned directly using standard git commands

- **Streaming & Compression**: All archives are compressed using zstd and streamed to/from storage

- **Queue System**: Configurable worker pool for processing archive requests

- **Modular Storage**: Interface-based storage system (filesystem now, S3 ready)

## Quick Start

1. **Start with Docker Compose**:
   ```bash
   docker compose up --build
   ```

2. **Access the application**:
   - Admin interface: http://localhost:8080/login
   - Default credentials: `admin/admin`

3. **Archive a URL**:
   - Use the admin interface to add a new URL
   - Or use the API: `POST /api/v1/archive` with `{"url": "https://example.com"}`

4. **View archives**:
   - Click on any short ID in the admin interface
   - Or visit directly: http://localhost:8080/{shortid}

## API

### Archive a URL
```bash
curl -X POST http://localhost:8080/api/v1/archive \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com", "types": ["mhtml", "screenshot"]}'
```

### Clone a Git Repository
```bash
git clone http://localhost:8080/git/{shortid}
```

## Configuration

Environment variables:

- `DB_URL`: PostgreSQL connection string
- `STORAGE_PATH`: Path for archive storage (default: `./storage`)
- `CACHE_PATH`: Path for git clone cache (default: `./cache`)
- `MAX_WORKERS`: Number of archive workers (default: `5`)

## Development

### Prerequisites
- Go 1.22+
- PostgreSQL
- Git
- Python 3 with yt-dlp
- Playwright dependencies

### Run locally
```bash
# Install dependencies
go mod tidy

# Start PostgreSQL (or use Docker)
docker run -d --name postgres \
  -e POSTGRES_USER=user \
  -e POSTGRES_PASSWORD=pass \
  -e POSTGRES_DB=arker \
  -p 5432:5432 postgres:15

# Install Playwright
go install github.com/playwright-community/playwright-go/cmd/playwright@latest
playwright install chromium

# Install yt-dlp
pip install yt-dlp

# Run the application
go run .
```

### Run tests
```bash
go test -v
```

## Architecture

The application uses:

- **Gin** for HTTP routing and middleware
- **GORM** for database operations with PostgreSQL
- **Playwright** for browser automation (MHTML, screenshots)
- **go-git** for Git repository operations
- **zstd** for streaming compression
- **Sessions** for admin authentication

Archive types are implemented using the `Archiver` interface, making it easy to add new archive strategies.

Storage uses the `Storage` interface, currently implemented for filesystem but designed for easy S3 integration.

## Docker Deployment

The included `docker-compose.yml` provides a complete deployment with PostgreSQL and proper resource limits for Playwright.

## Security Notes

- Change the default admin credentials in production
- Use a secure session key (update the hardcoded "secret-key-change-in-production")
- Consider adding rate limiting for the API endpoints
- Ensure proper network security for database access

## License

MIT License - See LICENSE file for details.
