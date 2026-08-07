package workers

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
)

func bandedImage(w, h int, top, bottom color.RGBA) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := top
		if y >= h/2 {
			c = bottom
		}
		for x := 0; x < w; x++ {
			m.Set(x, y, c)
		}
	}
	return m
}

// seedItem creates a capture with one archive item and returns the item.
func seedItem(t *testing.T, db *gorm.DB, shortID, typ, status, storageKey string) models.ArchiveItem {
	t.Helper()
	u := models.ArchivedURL{Original: "https://example.com/" + shortID}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	item := models.ArchiveItem{CaptureID: capture.ID, Type: typ, Status: status, StorageKey: storageKey}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
	return item
}

func putObject(t *testing.T, store storage.Storage, key string, data []byte) {
	t.Helper()
	w, err := store.Writer(key)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func reload(t *testing.T, db *gorm.DB, id uint) models.ArchiveItem {
	t.Helper()
	var got models.ArchiveItem
	if err := db.First(&got, id).Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	return got
}

func TestThumbnailWorkerGeneratesFromStoredScreenshot(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()

	// A stored screenshot is WebP, written by nativewebp.
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, bandedImage(1200, 3000, color.RGBA{220, 20, 20, 255}, color.RGBA{20, 20, 220, 255}), nil); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	key := "abc12/screenshot-deadbeef.webp"
	putObject(t, store, key, buf.Bytes())
	item := seedItem(t, db, "abc12", "screenshot", "completed", key)

	w := NewThumbnailWorker(store, db)
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "abc12", Type: "screenshot"}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	got := reload(t, db, item.ID)
	if got.ThumbnailStatus != models.ThumbnailStatusReady {
		t.Fatalf("status = %q, want ready", got.ThumbnailStatus)
	}
	if got.ThumbnailWidth != thumbnail.Width || got.ThumbnailHeight != thumbnail.Height {
		t.Errorf("dimensions = %dx%d, want %dx%d", got.ThumbnailWidth, got.ThumbnailHeight, thumbnail.Width, thumbnail.Height)
	}

	// The thumbnail key must be a new object, never an overwrite: the bucket
	// forbids replacing bytes in place.
	if got.ThumbnailKey == key {
		t.Fatal("thumbnail overwrote the source key")
	}
	r, err := store.Reader(got.ThumbnailKey)
	if err != nil {
		t.Fatalf("read thumbnail %q: %v", got.ThumbnailKey, err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("stored thumbnail is not decodable jpeg: %v", err)
	}
}

func TestThumbnailWorkerMarksNonImageTypesUnavailable(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	putObject(t, store, "abc12/mhtml-1.mhtml", []byte("not an image"))
	item := seedItem(t, db, "abc12", "mhtml", "completed", "abc12/mhtml-1.mhtml")

	w := NewThumbnailWorker(store, db)
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "abc12", Type: "mhtml"}); err != nil {
		t.Fatalf("generate should not error on an unsupported type: %v", err)
	}
	if got := reload(t, db, item.ID); got.ThumbnailStatus != models.ThumbnailStatusUnavailable {
		t.Errorf("status = %q, want unavailable so it is never re-enqueued", got.ThumbnailStatus)
	}
}

func TestThumbnailWorkerMarksUndecodableSourceUnavailable(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	putObject(t, store, "abc12/screenshot-1.webp", []byte("corrupt bytes, not an image"))
	item := seedItem(t, db, "abc12", "screenshot", "completed", "abc12/screenshot-1.webp")

	w := NewThumbnailWorker(store, db)
	// Must not return an error: retrying cannot change the outcome, and River
	// would otherwise burn attempts on it.
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "abc12", Type: "screenshot"}); err != nil {
		t.Fatalf("generate should swallow permanent decode failures: %v", err)
	}
	if got := reload(t, db, item.ID); got.ThumbnailStatus != models.ThumbnailStatusUnavailable {
		t.Errorf("status = %q, want unavailable", got.ThumbnailStatus)
	}
}

func TestThumbnailWorkerLeavesIncompleteItemsUnmarked(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	item := seedItem(t, db, "abc12", "screenshot", "processing", "")

	w := NewThumbnailWorker(store, db)
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "abc12", Type: "screenshot"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Status must stay empty, not "unavailable": the capture is still running
	// and will be thumbnailable once it lands.
	if got := reload(t, db, item.ID); got.ThumbnailStatus != "" {
		t.Errorf("status = %q, want empty so a later view retries", got.ThumbnailStatus)
	}
}

func TestThumbnailWorkerIsIdempotent(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	item := seedItem(t, db, "abc12", "screenshot", "completed", "abc12/screenshot-1.webp")
	db.Model(&item).Updates(map[string]interface{}{
		"thumbnail_key":    "abc12/screenshot-1-thumb.jpg",
		"thumbnail_status": models.ThumbnailStatusReady,
	})

	// No source object exists, so any attempt to regenerate would error.
	w := NewThumbnailWorker(store, db)
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "abc12", Type: "screenshot"}); err != nil {
		t.Fatalf("generate on an already-ready item should be a no-op: %v", err)
	}
	if got := reload(t, db, item.ID); got.ThumbnailKey != "abc12/screenshot-1-thumb.jpg" {
		t.Errorf("key changed to %q, want the existing thumbnail untouched", got.ThumbnailKey)
	}
}

func TestThumbnailWorkerToleratesMissingItem(t *testing.T) {
	db := newWorkerTestDB(t)
	w := NewThumbnailWorker(storage.NewMemoryStorage(), db)
	if err := w.generate(context.Background(), ThumbnailJobArgs{ShortID: "nope1", Type: "screenshot"}); err != nil {
		t.Fatalf("a deleted archive should drop the job, not error: %v", err)
	}
}

// The inline path: an archiver that returns a thumbnail gets it persisted as
// part of the normal archive job.
func TestProcessArchiveJobPersistsInlineThumbnail(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	item := seedItem(t, db, "abc12", "screenshot", "processing", "")

	thumb, err := thumbnail.FromImage(bandedImage(1200, 2400, color.RGBA{10, 200, 10, 255}, color.RGBA{10, 10, 200, 255}))
	if err != nil {
		t.Fatalf("build thumbnail: %v", err)
	}

	m := map[string]archivers.Archiver{"screenshot": thumbArchiver{
		payload: []byte("fake screenshot bytes"),
		thumb:   &archivers.Thumbnail{Data: thumb.Data, Width: thumb.Width, Height: thumb.Height},
	}}
	args := ArchiveJobArgs{ShortID: "abc12", Type: "screenshot", URL: "https://example.com/abc12"}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	got := reload(t, db, item.ID)
	if got.Status != "completed" {
		t.Fatalf("archive status = %q, want completed", got.Status)
	}
	if got.ThumbnailStatus != models.ThumbnailStatusReady || got.ThumbnailKey == "" {
		t.Fatalf("thumbnail not persisted: status=%q key=%q", got.ThumbnailStatus, got.ThumbnailKey)
	}
	if exists, _ := store.Exists(got.ThumbnailKey); !exists {
		t.Errorf("thumbnail object %q was not written", got.ThumbnailKey)
	}
}

// A thumbnail that cannot be stored must not fail an otherwise-good archive.
func TestProcessArchiveJobSurvivesThumbnailStorageFailure(t *testing.T) {
	db := newWorkerTestDB(t)
	store := &thumbFailingStorage{Storage: storage.NewMemoryStorage()}
	item := seedItem(t, db, "abc12", "screenshot", "processing", "")

	m := map[string]archivers.Archiver{"screenshot": thumbArchiver{
		payload: []byte("fake screenshot bytes"),
		thumb:   &archivers.Thumbnail{Data: []byte("jpeg-ish"), Width: 480, Height: 270},
	}}
	args := ArchiveJobArgs{ShortID: "abc12", Type: "screenshot", URL: "https://example.com/abc12"}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("a thumbnail failure must not fail the archive: %v", err)
	}

	got := reload(t, db, item.ID)
	if got.Status != "completed" {
		t.Errorf("archive status = %q, want completed", got.Status)
	}
	if got.ThumbnailStatus == models.ThumbnailStatusReady {
		t.Error("thumbnail marked ready despite the write failing")
	}
}

type thumbArchiver struct {
	payload []byte
	thumb   *archivers.Thumbnail
}

func (a thumbArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	return archivers.Result{
		Data:        bytes.NewReader(a.payload),
		Extension:   ".webp",
		ContentType: "image/webp",
		Thumbnail:   a.thumb,
	}, nil
}

// thumbFailingStorage accepts the archive object but rejects the thumbnail.
type thumbFailingStorage struct {
	storage.Storage
}

func (s *thumbFailingStorage) Writer(key string) (io.WriteCloser, error) {
	if bytes.Contains([]byte(key), []byte("-thumb")) {
		return nil, io.ErrClosedPipe
	}
	return s.Storage.Writer(key)
}
