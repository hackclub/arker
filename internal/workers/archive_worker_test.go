package workers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type stubArchiver struct{ err error }

func (s stubArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *archivers.PWBundle, error) {
	return nil, "", "", nil, s.err
}

// dataArchiver is a stub that returns fixed content and no error.
type dataArchiver struct{ payload []byte }

func (d dataArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *archivers.PWBundle, error) {
	return bytes.NewReader(d.payload), ".mhtml", "application/x-mhtml", nil, nil
}

func newWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestProcessArchiveJobDoesNotMarkFailedOnRetryableError verifies a failed
// attempt leaves the item in "processing", not "failed" (regression: premature
// failed status during retry backoff).
func TestProcessArchiveJobDoesNotMarkFailedOnRetryableError(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "abc12"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": stubArchiver{err: errors.New("boom")}}
	args := ArchiveJobArgs{ShortID: "abc12", Type: "mhtml", URL: "https://example.com"}

	err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m)
	if err == nil {
		t.Fatal("expected an error from the stub archiver")
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status == "failed" {
		t.Fatalf("item was marked failed on a retryable attempt; want processing, got %q", got.Status)
	}
}

func TestUploadNonceIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		n := uploadNonce()
		if n == "" {
			t.Fatal("uploadNonce returned empty string")
		}
		if seen[n] {
			t.Fatalf("uploadNonce collision: %q", n)
		}
		seen[n] = true
	}
}

func TestSaveArchiveDataMarksCompleted(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv001"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	payload := []byte("hello-archive")
	key := "sv001/mhtml-deadbeef.mhtml"
	if err := saveArchiveData(bytes.NewReader(payload), key, ".mhtml", store, db, &item); err != nil {
		t.Fatalf("saveArchiveData: %v", err)
	}

	// Storage got the bytes.
	if size, err := store.Size(key); err != nil || size != int64(len(payload)) {
		t.Fatalf("stored size = %d, err %v; want %d", size, err, len(payload))
	}
	// Item is completed with the right metadata.
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey != key || got.Extension != ".mhtml" || got.FileSize != int64(len(payload)) {
		t.Fatalf("item not finalized correctly: %+v", got)
	}
}

func TestProcessArchiveJobSuccessCompletes(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv002"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": dataArchiver{payload: []byte("body")}}
	args := ArchiveJobArgs{ShortID: "sv002", Type: "mhtml", URL: "https://example.com"}

	if err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.StorageKey == "" {
		t.Fatal("storage key not set on completion")
	}
}
