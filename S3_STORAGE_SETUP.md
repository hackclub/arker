# S3 Storage Backend Configuration

Arker now supports S3-compatible storage as an alternative to local filesystem storage. This includes AWS S3, MinIO, Backblaze B2, DigitalOcean Spaces, and other S3-compatible services.

## Environment Variables

To enable S3 storage, set the following environment variables:

### Required Variables

```bash
# Enable S3 storage
STORAGE_TYPE=s3

# S3 bucket name (required)
S3_BUCKET=your-bucket-name

# AWS Region (defaults to us-east-1)
S3_REGION=us-east-1
```

### Authentication

**Option 1: Static Credentials**
```bash
S3_ACCESS_KEY_ID=your-access-key
S3_SECRET_ACCESS_KEY=your-secret-key
```

**Option 2: Default AWS Credentials Chain**
If you don't set `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY`, Arker will use the AWS default credentials chain:
- Environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`)
- IAM roles (if running on EC2)
- AWS CLI credentials (`~/.aws/credentials`)

### Optional Variables

```bash
# Custom S3 endpoint (for S3-compatible services like MinIO)
S3_ENDPOINT=https://s3.your-provider.com

# Key prefix for organization (e.g., "arker/" or "production/archives/")
S3_PREFIX=arker/

# Force path-style URLs (required for MinIO, some providers)
S3_FORCE_PATH_STYLE=true
```

## Configuration Examples

### AWS S3
```bash
STORAGE_TYPE=s3
S3_BUCKET=my-arker-archives
S3_REGION=us-west-2
S3_ACCESS_KEY_ID=AKIA...
S3_SECRET_ACCESS_KEY=...
```

### MinIO
```bash
STORAGE_TYPE=s3
S3_ENDPOINT=https://minio.example.com
S3_BUCKET=arker
S3_REGION=us-east-1
S3_ACCESS_KEY_ID=minioadmin
S3_SECRET_ACCESS_KEY=minioadmin
S3_FORCE_PATH_STYLE=true
```

### Backblaze B2
```bash
STORAGE_TYPE=s3
S3_ENDPOINT=https://s3.us-west-002.backblazeb2.com
S3_BUCKET=my-arker-bucket
S3_REGION=us-west-002
S3_ACCESS_KEY_ID=your-b2-key-id
S3_SECRET_ACCESS_KEY=your-b2-application-key
S3_FORCE_PATH_STYLE=false
```

### DigitalOcean Spaces
```bash
STORAGE_TYPE=s3
S3_ENDPOINT=https://nyc3.digitaloceanspaces.com
S3_BUCKET=my-space-name
S3_REGION=nyc3
S3_ACCESS_KEY_ID=your-spaces-key
S3_SECRET_ACCESS_KEY=your-spaces-secret
```

## Docker Compose Example

```yaml
version: '3.8'
services:
  arker:
    image: arker:latest
    environment:
      - STORAGE_TYPE=s3
      - S3_BUCKET=my-arker-archives
      - S3_REGION=us-west-2
      - S3_ACCESS_KEY_ID=AKIA...
      - S3_SECRET_ACCESS_KEY=...
      - S3_PREFIX=production/
      - DB_URL=postgres://user:pass@db:5432/arker?sslmode=disable
    ports:
      - "8080:8080"
```

## Testing with MinIO (Local Development)

For local development and testing, you can use MinIO:

```bash
# Start MinIO container
docker run -d \
  --name minio \
  -p 9000:9000 \
  -p 9001:9001 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  quay.io/minio/minio server /data --console-address ":9001"

# Create bucket (visit http://localhost:9001, login: minioadmin/minioadmin)
# Or use mc CLI tool

# Run Arker with MinIO
export STORAGE_TYPE=s3
export S3_ENDPOINT=http://localhost:9000
export S3_BUCKET=arker
export S3_REGION=us-east-1
export S3_ACCESS_KEY_ID=minioadmin
export S3_SECRET_ACCESS_KEY=minioadmin
export S3_FORCE_PATH_STYLE=true

make dev  # or go run .
```

## Features

- **Compression**: All files stored in S3 are compressed using zstd (same as filesystem storage)
- **Range Requests**: Supports seeking through large files using HTTP range requests
- **Concurrent Access**: Safe for multiple Arker instances accessing the same bucket
- **Path Organization**: Optional prefix support for organizing files within buckets
- **Error Handling**: Proper error handling for network issues, permissions, etc.

## Important Notes

1. **Bucket Permissions**: Ensure your credentials have `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, and `s3:ListBucket` permissions
2. **Costs**: Be aware of storage and data transfer costs with your S3 provider
3. **Performance**: S3 storage may be slower than local filesystem for frequent small file access
4. **Compression**: Files are still compressed with zstd before uploading to S3
5. **Migration**: To migrate from filesystem to S3 storage, you'll need to upload existing files manually

## Troubleshooting

### Common Issues

- **"Access Denied"**: Check your credentials and bucket permissions
- **"NoSuchBucket"**: Ensure the bucket exists and the region is correct
- **Connection timeouts**: Check your endpoint URL and network connectivity
- **"InvalidRequest"**: Try setting `S3_FORCE_PATH_STYLE=true` for MinIO/compatible services

### Logs

Arker will log storage initialization and any S3-related errors. Check the application logs for details about storage operations.
