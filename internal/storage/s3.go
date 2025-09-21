package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storage implements the Storage interface for S3-compatible storage
type S3Storage struct {
	client  *s3.Client
	bucket  string
	prefix  string // Optional prefix for all keys
	tempDir string // Temp directory for upload buffering
}

// S3Config holds configuration for S3 storage
type S3Config struct {
	Endpoint        string // For S3-compatible services like MinIO, B2, etc.
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Prefix          string // Optional prefix for all keys (e.g., "arker/")
	ForcePathStyle  bool   // Required for MinIO and some S3-compatible services
	TempDir         string // Temp directory for upload buffering
}

// NewS3Storage creates a new S3 storage instance
func NewS3Storage(ctx context.Context, cfg S3Config) (*S3Storage, error) {
	// Load AWS config
	var awsConfig aws.Config
	var err error

	// Create credentials if provided
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		awsConfig, err = config.LoadDefaultConfig(ctx,
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			)),
			config.WithRegion(cfg.Region),
		)
	} else {
		// Use default credentials (environment, IAM role, etc.)
		awsConfig, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(cfg.Region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client options
	s3Options := func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	}

	client := s3.NewFromConfig(awsConfig, s3Options)

	return &S3Storage{
		client:  client,
		bucket:  cfg.Bucket,
		prefix:  cfg.Prefix,
		tempDir: cfg.TempDir,
	}, nil
}

// buildKey adds the prefix to a key if configured
func (s *S3Storage) buildKey(key string) string {
	if s.prefix == "" {
		return key
	}
	// Ensure prefix ends with / if it doesn't already
	prefix := s.prefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + key
}

// Writer creates a writer for the given key
func (s *S3Storage) Writer(key string) (io.WriteCloser, error) {
	// Create temporary file for buffering
	tempFile, err := os.CreateTemp(s.tempDir, "arker-s3-upload-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	writer := &s3Writer{
		storage:  s,
		key:      s.buildKey(key),
		tempFile: tempFile,
		tempPath: tempFile.Name(),
	}
	
	// Set finalizer to ensure cleanup if Close() is never called
	runtime.SetFinalizer(writer, (*s3Writer).cleanup)
	
	return writer, nil
}

// Reader creates a reader for the given key
func (s *S3Storage) Reader(key string) (io.ReadCloser, error) {
	ctx := context.Background()
	
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}
	
	return result.Body, nil
}

// Exists checks if an object exists
func (s *S3Storage) Exists(key string) (bool, error) {
	ctx := context.Background()
	
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(key)),
	})
	
	if err != nil {
		var noSuchKey *types.NoSuchKey
		var notFound *types.NotFound
		if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check if object exists: %w", err)
	}
	
	return true, nil
}

// Size returns the size of an object
func (s *S3Storage) Size(key string) (int64, error) {
	ctx := context.Background()
	
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(key)),
	})
	
	if err != nil {
		return 0, fmt.Errorf("failed to get object metadata: %w", err)
	}
	
	if result.ContentLength == nil {
		return 0, nil
	}
	
	return *result.ContentLength, nil
}

// SeekableReader creates a seekable reader (not fully seekable for S3, but supports range reads)  
func (s *S3Storage) SeekableReader(key string) (ReadSeekCloser, error) {
	return &s3SeekableReader{
		storage: s,
		key:     s.buildKey(key),
		pos:     0,
	}, nil
}

// s3Writer implements io.WriteCloser for S3 uploads
type s3Writer struct {
	storage   *S3Storage
	key       string
	tempFile  *os.File
	tempPath  string
	closed    bool
	cleanedUp bool
	mu        sync.Mutex
}

func (w *s3Writer) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	
	if w.closed {
		return 0, fmt.Errorf("cannot write to closed writer")
	}
	
	n, err = w.tempFile.Write(p)
	if err != nil {
		// If write fails, cleanup immediately
		w.cleanup()
	}
	return n, err
}

func (w *s3Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	
	if w.closed {
		return nil // Already closed
	}
	w.closed = true
	
	// Clear finalizer since we're handling cleanup properly
	runtime.SetFinalizer(w, nil)
	
	// Ensure cleanup happens regardless of success/failure
	defer w.cleanup()
	
	// Close the temp file first
	if err := w.tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	
	// Open file for reading and upload
	file, err := os.Open(w.tempPath)
	if err != nil {
		return fmt.Errorf("failed to reopen temp file: %w", err)
	}
	defer file.Close()
	
	ctx := context.Background()
	_, err = w.storage.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(w.storage.bucket),
		Key:    aws.String(w.key),
		Body:   file, // Stream directly from disk
	})
	
	if err != nil {
		return fmt.Errorf("failed to upload object to S3: %w", err)
	}
	
	return nil
}

// cleanup removes the temporary file - safe to call multiple times
func (w *s3Writer) cleanup() {
	if w.cleanedUp {
		return
	}
	w.cleanedUp = true
	
	// Close file if still open
	if w.tempFile != nil {
		w.tempFile.Close()
	}
	
	// Remove temp file
	if w.tempPath != "" {
		os.Remove(w.tempPath)
	}
}

// s3SeekableReader implements a seekable reader for S3 objects using range requests
type s3SeekableReader struct {
	storage *S3Storage
	key     string
	pos     int64
	reader  io.ReadCloser
	size    int64
	sizeSet bool
}

func (r *s3SeekableReader) Read(p []byte) (n int, err error) {
	if r.reader == nil {
		if err := r.openReader(); err != nil {
			return 0, err
		}
	}
	
	n, err = r.reader.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *s3SeekableReader) Seek(offset int64, whence int) (int64, error) {
	// Close current reader
	if r.reader != nil {
		r.reader.Close()
		r.reader = nil
	}
	
	// Get object size if not set
	if !r.sizeSet {
		size, err := r.storage.Size(strings.TrimPrefix(r.key, r.storage.prefix))
		if err != nil {
			return 0, err
		}
		r.size = size
		r.sizeSet = true
	}
	
	// Calculate new position
	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence value")
	}
	
	// Ensure position is valid
	if r.pos < 0 {
		r.pos = 0
	}
	if r.pos > r.size {
		r.pos = r.size
	}
	
	return r.pos, nil
}

func (r *s3SeekableReader) Close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

func (r *s3SeekableReader) openReader() error {
	ctx := context.Background()
	
	var rangeHeader *string
	if r.pos > 0 {
		rangeHeader = aws.String(fmt.Sprintf("bytes=%d-", r.pos))
	}
	
	result, err := r.storage.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.storage.bucket),
		Key:    aws.String(r.key),
		Range:  rangeHeader,
	})
	
	if err != nil {
		return fmt.Errorf("failed to get object from S3: %w", err)
	}
	
	r.reader = result.Body
	return nil
}
