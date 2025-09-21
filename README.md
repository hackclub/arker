# Arker

A self-hostable minimalist version of <https://archive.org>.

- Creates Chrome snapshots of URLs and serves them at nice short URLs like <https://archive.hackclub.com/p9OGi>
- Also supports git clones, YouTube videos, and website screenshots
- Comprehensive API

- Stores everything compressed using [zstd](https://github.com/facebook/zstd) level 6 (seekable format for random access)
- Flexible storage: local filesystem or S3-compatible cloud storage

Try out the demo instance at <https://arker-demo.hackclub.com>.

## Configuration

### Basic Settings

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


## License

MIT
