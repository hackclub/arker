package storage

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestS3StorageDirectURLUsesExactStorageKeyWithPublicBaseURL(t *testing.T) {
	storageInstance := &S3Storage{
		publicBaseURL: "https://objects.example.com/files",
		prefix:        "arker",
	}

	directURL, err := storageInstance.DirectURL(context.Background(), "archive/Gxrbu/youtube.mp4", DirectURLOptions{
		ContentType:        "video/mp4",
		ContentDisposition: `attachment; filename="youtube.mp4"`,
	})
	if err != nil {
		t.Fatalf("DirectURL returned error: %v", err)
	}

	parsed, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("failed to parse direct URL %q: %v", directURL, err)
	}
	if got, want := parsed.Path, "/files/arker/archive/Gxrbu/youtube.mp4"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	query := parsed.Query()
	if got := query.Get("response-content-type"); got != "video/mp4" {
		t.Fatalf("response-content-type = %q, want %q", got, "video/mp4")
	}
	if got := query.Get("response-content-disposition"); got != `attachment; filename="youtube.mp4"` {
		t.Fatalf("response-content-disposition = %q, want attachment filename override", got)
	}
}

func TestS3StorageDirectURLPresignsExactStorageKey(t *testing.T) {
	storageInstance, err := NewS3Storage(context.Background(), S3Config{
		Endpoint:        "https://s3.example.com",
		Region:          "us-east-1",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Bucket:          "archive-bucket",
		Prefix:          "arker",
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage returned error: %v", err)
	}

	directURL, err := storageInstance.DirectURL(context.Background(), "archive/Gxrbu/youtube.mp4", DirectURLOptions{
		ContentType: "video/mp4",
	})
	if err != nil {
		t.Fatalf("DirectURL returned error: %v", err)
	}

	parsed, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("failed to parse direct URL %q: %v", directURL, err)
	}
	if got, want := parsed.Path, "/archive-bucket/arker/archive/Gxrbu/youtube.mp4"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}

	query := parsed.Query()
	if query.Get("X-Amz-Signature") == "" {
		t.Fatal("presigned URL is missing X-Amz-Signature")
	}
	if got := query.Get("X-Amz-Expires"); got != "43200" {
		t.Fatalf("X-Amz-Expires = %q, want %q", got, "43200")
	}
	if got := query.Get("response-content-type"); got != "video/mp4" {
		t.Fatalf("response-content-type = %q, want %q", got, "video/mp4")
	}
	if got := query.Get("x-id"); got != "GetObject" {
		t.Fatalf("x-id = %q, want %q", got, "GetObject")
	}
}

func TestS3StorageDirectURLPresignsHeadRequests(t *testing.T) {
	storageInstance, err := NewS3Storage(context.Background(), S3Config{
		Endpoint:        "https://s3.example.com",
		Region:          "us-east-1",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Bucket:          "archive-bucket",
		Prefix:          "arker",
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage returned error: %v", err)
	}

	directURL, err := storageInstance.DirectURL(context.Background(), "archive/Gxrbu/youtube.mp4", DirectURLOptions{
		Method:      "HEAD",
		ContentType: "video/mp4",
	})
	if err != nil {
		t.Fatalf("DirectURL returned error: %v", err)
	}

	parsed, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("failed to parse direct URL %q: %v", directURL, err)
	}
	if got, want := parsed.Path, "/archive-bucket/arker/archive/Gxrbu/youtube.mp4"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}

	query := parsed.Query()
	if query.Get("X-Amz-Signature") == "" {
		t.Fatal("presigned URL is missing X-Amz-Signature")
	}
	if got := query.Get("x-id"); got == "GetObject" {
		t.Fatalf("x-id = %q, want a HEAD presign URL rather than GetObject", got)
	}
	if got := query.Get("response-content-type"); got != "video/mp4" {
		t.Fatalf("response-content-type = %q, want %q", got, "video/mp4")
	}
}

func TestS3StorageDirectURLUsesConfiguredExpiration(t *testing.T) {
	storageInstance, err := NewS3Storage(context.Background(), S3Config{
		Endpoint:            "https://s3.example.com",
		Region:              "us-east-1",
		AccessKeyID:         "test-access-key",
		SecretAccessKey:     "test-secret-key",
		Bucket:              "archive-bucket",
		DirectURLExpiration: 6 * time.Hour,
		ForcePathStyle:      true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage returned error: %v", err)
	}

	directURL, err := storageInstance.DirectURL(context.Background(), "archive/Gxrbu/youtube.mp4", DirectURLOptions{})
	if err != nil {
		t.Fatalf("DirectURL returned error: %v", err)
	}

	parsed, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("failed to parse direct URL %q: %v", directURL, err)
	}
	if got := parsed.Query().Get("X-Amz-Expires"); got != "21600" {
		t.Fatalf("X-Amz-Expires = %q, want %q", got, "21600")
	}
}
