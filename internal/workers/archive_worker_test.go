package workers

import (
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
