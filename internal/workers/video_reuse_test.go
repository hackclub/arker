package workers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"

	"gorm.io/gorm"
)

// refreshTrackingArchiver counts which flow the worker chose: a full download
// (Archive) or a metadata-only refresh (RefreshVideoMetadata).
type refreshTrackingArchiver struct {
	archiveCalls int
	refreshCalls int
	refreshErr   error
	refreshMedia archivers.VideoMedia
}

func (a *refreshTrackingArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	a.archiveCalls++
	return archivers.Result{
		Data:         bytes.NewReader([]byte("fresh-mp4")),
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Metadata:     &archivers.Sidecar{Data: []byte(`{"schema_version":"1","title":"Downloaded"}`)},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"id":"full"}`)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

func (a *refreshTrackingArchiver) RefreshVideoMetadata(ctx context.Context, url string, logWriter io.Writer, media archivers.VideoMedia) (archivers.Result, error) {
	a.refreshCalls++
	a.refreshMedia = media
	if a.refreshErr != nil {
		return archivers.Result{}, a.refreshErr
	}
	return archivers.Result{
		Extension:    media.Extension,
		ContentType:  "video/mp4",
		Metadata:     &archivers.Sidecar{Data: []byte(`{"schema_version":"1","title":"Refreshed"}`)},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"id":"refreshed"}`)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// seedPriorVideoCapture stores a completed yt-dlp capture of url along with
// its media object, and returns the prior item.
func seedPriorVideoCapture(t *testing.T, db *gorm.DB, store storage.Storage, url, shortID string, mutate func(*models.ArchiveItem)) models.ArchiveItem {
	t.Helper()
	archived := models.ArchivedURL{Original: url, CanonicalURL: utils.CanonicalizeArchiveURL(url)}
	db.Create(&archived)
	capture := models.Capture{ArchivedURLID: archived.ID, Timestamp: time.Now().Add(-time.Hour), ShortID: shortID}
	db.Create(&capture)

	storageKey := shortID + "/yt-dlp-aaaa1111.mp4"
	w, err := store.Writer(storageKey)
	if err != nil {
		t.Fatalf("store writer: %v", err)
	}
	if _, err := w.Write([]byte("stored-mp4")); err != nil {
		t.Fatalf("store write: %v", err)
	}
	w.Close()

	item := models.ArchiveItem{
		CaptureID:       capture.ID,
		Type:            "yt-dlp",
		Status:          "completed",
		StorageKey:      storageKey,
		Extension:       ".mp4",
		FileSize:        10,
		MetadataKey:     shortID + "/yt-dlp-aaaa1111.metadata.json",
		RawMetadataKey:  shortID + "/yt-dlp-aaaa1111.raw-metadata.json",
		Completeness:    archivers.CompletenessComplete,
		Source:          models.ArchiveSourceNative,
		ThumbnailKey:    shortID + "/yt-dlp-aaaa1111-thumb.webp",
		ThumbnailWidth:  320,
		ThumbnailHeight: 180,
		ThumbnailStatus: models.ThumbnailStatusReady,
	}
	if mutate != nil {
		mutate(&item)
	}
	db.Create(&item)
	return item
}

func newPendingVideoItem(t *testing.T, db *gorm.DB, url, shortID string) models.ArchiveItem {
	t.Helper()
	var archived models.ArchivedURL
	if err := db.Where("original = ?", url).First(&archived).Error; err != nil {
		archived = models.ArchivedURL{Original: url, CanonicalURL: utils.CanonicalizeArchiveURL(url)}
		db.Create(&archived)
	}
	capture := models.Capture{ArchivedURLID: archived.ID, Timestamp: time.Now(), ShortID: shortID}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "yt-dlp", Status: "processing"}
	db.Create(&item)
	return item
}

// A repeat capture of an already-archived video must not download the bytes
// again: the media object is shared, and only the metadata is refreshed.
func TestRepeatVideoCaptureReusesStoredMediaAndRefreshesMetadata(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	prior := seedPriorVideoCapture(t, db, store, url, "old01", nil)
	item := newPendingVideoItem(t, db, url, "new01")

	arch := &refreshTrackingArchiver{}
	m := map[string]archivers.Archiver{"yt-dlp": arch}
	args := ArchiveJobArgs{ShortID: "new01", Type: "yt-dlp", URL: url}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	if arch.archiveCalls != 0 {
		t.Errorf("the video was downloaded again: Archive called %d time(s)", arch.archiveCalls)
	}
	if arch.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", arch.refreshCalls)
	}
	if arch.refreshMedia.Extension != ".mp4" || arch.refreshMedia.SizeBytes != prior.FileSize {
		t.Errorf("refresh was not told about the stored media: %+v", arch.refreshMedia)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.StorageKey != prior.StorageKey {
		t.Errorf("storage key = %q, want the shared media object %q", got.StorageKey, prior.StorageKey)
	}
	if got.FileSize != prior.FileSize || got.Extension != prior.Extension {
		t.Errorf("media facts not inherited: %+v", got)
	}
	if got.Source != models.ArchiveSourceNative {
		t.Errorf("source = %q, want the stored media's provenance %q", got.Source, models.ArchiveSourceNative)
	}
	if got.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q, want complete", got.Completeness)
	}

	// The sidecars are fresh and live under the new capture's key base.
	if got.MetadataKey == "" || got.MetadataKey == prior.MetadataKey || !strings.HasPrefix(got.MetadataKey, "new01/") {
		t.Fatalf("metadata key = %q, want a fresh sidecar under new01/", got.MetadataKey)
	}
	if got.RawMetadataKey == "" || got.RawMetadataKey == prior.RawMetadataKey || !strings.HasPrefix(got.RawMetadataKey, "new01/") {
		t.Fatalf("raw metadata key = %q, want a fresh sidecar under new01/", got.RawMetadataKey)
	}
	reader, err := store.Reader(got.MetadataKey)
	if err != nil {
		t.Fatalf("read refreshed metadata: %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if !strings.Contains(string(data), "Refreshed") {
		t.Errorf("stored metadata = %q, want the refreshed record", data)
	}

	// The preview is shared with the earlier capture when the refresh brought
	// none of its own.
	if got.ThumbnailKey != prior.ThumbnailKey || got.ThumbnailStatus != models.ThumbnailStatusReady {
		t.Errorf("thumbnail not inherited: key=%q status=%q", got.ThumbnailKey, got.ThumbnailStatus)
	}
}

// When even the metadata refresh fails (deleted post, platform refusal), the
// capture still completes with the earlier sidecars: the archive already holds
// the product, and failing the item would claim otherwise.
func TestRepeatVideoCaptureInheritsSidecarsWhenRefreshFails(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	prior := seedPriorVideoCapture(t, db, store, url, "old02", nil)
	item := newPendingVideoItem(t, db, url, "new02")

	arch := &refreshTrackingArchiver{refreshErr: errors.New("video unavailable")}
	m := map[string]archivers.Archiver{"yt-dlp": arch}
	args := ArchiveJobArgs{ShortID: "new02", Type: "yt-dlp", URL: url}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	if arch.archiveCalls != 0 {
		t.Errorf("fell back to a full download despite stored sidecars: Archive called %d time(s)", arch.archiveCalls)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey != prior.StorageKey {
		t.Fatalf("item not completed from stored media: %+v", got)
	}
	if got.MetadataKey != prior.MetadataKey || got.RawMetadataKey != prior.RawMetadataKey {
		t.Errorf("sidecars not inherited: %+v", got)
	}
}

// A legacy prior item that never stored sidecars cannot satisfy a new capture
// when the refresh fails: only a full run can produce metadata, so it runs.
func TestRepeatVideoCaptureFallsBackToFullRunWithoutStoredMetadata(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	prior := seedPriorVideoCapture(t, db, store, url, "old03", func(item *models.ArchiveItem) {
		item.MetadataKey = ""
		item.RawMetadataKey = ""
	})
	item := newPendingVideoItem(t, db, url, "new03")

	arch := &refreshTrackingArchiver{refreshErr: errors.New("video unavailable")}
	m := map[string]archivers.Archiver{"yt-dlp": arch}
	args := ArchiveJobArgs{ShortID: "new03", Type: "yt-dlp", URL: url}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	if arch.archiveCalls != 1 {
		t.Fatalf("Archive calls = %d, want 1 (full run is the only path to metadata)", arch.archiveCalls)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.StorageKey == prior.StorageKey || !strings.HasPrefix(got.StorageKey, "new03/") {
		t.Errorf("storage key = %q, want a freshly downloaded object", got.StorageKey)
	}
}

// The stored video answers a repeat request made under a different spelling of
// the same post, and a pre-rename "youtube" row keeps its video reusable.
func TestFindReusableVideoItemMatchesSpellingsAndLegacyType(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	priorURL := "https://youtu.be/dQw4w9WgXcQ"
	requestURL := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if utils.CanonicalizeArchiveURL(priorURL) != utils.CanonicalizeArchiveURL(requestURL) {
		t.Fatal("test fixture: the two spellings must share a canonical identity")
	}

	prior := seedPriorVideoCapture(t, db, store, priorURL, "old04", func(item *models.ArchiveItem) {
		item.Type = "youtube" // pre-rename spelling
	})

	found := findReusableVideoItem(db, requestURL, 0)
	if found == nil || found.ID != prior.ID {
		t.Fatalf("findReusableVideoItem = %+v, want the legacy item %d", found, prior.ID)
	}
}

// Failed items and items whose media never made it to storage must not be
// reused, and the item being processed must never match itself.
func TestFindReusableVideoItemIgnoresUnusableItems(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	failed := seedPriorVideoCapture(t, db, store, url, "old05", func(item *models.ArchiveItem) {
		item.Status = "failed"
	})
	if found := findReusableVideoItem(db, url, 0); found != nil {
		t.Fatalf("reused a failed item: %+v", found)
	}

	db.Model(&models.ArchiveItem{}).Where("id = ?", failed.ID).
		Updates(map[string]interface{}{"status": "completed", "storage_key": ""})
	if found := findReusableVideoItem(db, url, 0); found != nil {
		t.Fatalf("reused an item with no stored media: %+v", found)
	}

	db.Model(&models.ArchiveItem{}).Where("id = ?", failed.ID).
		Update("storage_key", "old05/yt-dlp-aaaa1111.mp4")
	if found := findReusableVideoItem(db, url, failed.ID); found != nil {
		t.Fatalf("an item matched itself: %+v", found)
	}
	if found := findReusableVideoItem(db, url, 0); found == nil || found.ID != failed.ID {
		t.Fatalf("a completed stored item was not found: %+v", found)
	}
}

// A Bright Data rescue is a degraded 360p stand-in, not the bytes a native run
// would fetch — a repeat capture must redownload, not alias the rescue.
func TestFindReusableVideoItemSkipsBrightDataRescues(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	rescued := seedPriorVideoCapture(t, db, store, url, "old06", func(item *models.ArchiveItem) {
		item.Source = models.ArchiveSourceBrightData
	})
	if found := findReusableVideoItem(db, url, 0); found != nil {
		t.Fatalf("reused a Bright Data rescue: %+v", found)
	}

	db.Model(&models.ArchiveItem{}).Where("id = ?", rescued.ID).
		Update("source", models.ArchiveSourceNative)
	if found := findReusableVideoItem(db, url, 0); found == nil || found.ID != rescued.ID {
		t.Fatalf("a native stored item was not found: %+v", found)
	}
}
