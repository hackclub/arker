# Arker

A self-hostable minimalist version of <https://archive.org>.

- Creates Chrome snapshots of URLs and serves them at nice short URLs like <https://archive.hackclub.com/p9OGi>
- Also supports git clones, YouTube videos, and website screenshots
- Comprehensive API

- Stores everything compressed on disk using [zstd](https://github.com/facebook/zstd)

Try out the demo instance at <https://arker-demo.hackclub.com>.

## Configuration

- `DB_URL` - PostgreSQL connection string (default: `host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable`)
- `STORAGE_PATH` - Archive storage directory (default: `./storage`)
- `CACHE_PATH` - Git clone cache directory (default: `./cache`)
- `MAX_WORKERS` - Worker pool size (default: `5`)
- `PORT` - HTTP server port (default: `8080`)
- `SESSION_SECRET` - Session encryption key (auto-generated if not set)
- `ADMIN_USERNAME` - Admin login username (default: `admin`)
- `ADMIN_PASSWORD` - Admin login password (default: `admin`)
- `LOGIN_TEXT` - Custom text to display under the login form. Useful for providing demo credentials (e.g., `LOGIN_TEXT="Demo: admin/admin"`). Supports basic HTML.
- `GIN_MODE` - Gin framework mode (`debug` for development)


## License

MIT
