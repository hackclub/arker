package workers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

type fakeSocialRefresher struct {
	thumb *archivers.Thumbnail
	err   error
	calls int
}

func (f *fakeSocialRefresher) RefreshSocialThumbnail(context.Context, string, io.Writer) (*archivers.Thumbnail, error) {
	f.calls++
	return f.thumb, f.err
}

type fakeSocialProvider struct {
	thumb     *archivers.Thumbnail
	err       error
	cost      float64
	calls     int
	supported bool
}

func (f *fakeSocialProvider) ResolveSocialThumbnail(context.Context, string, string, io.Writer, *gorm.DB, uint) (*archivers.Thumbnail, error) {
	f.calls++
	return f.thumb, f.err
}

func (f *fakeSocialProvider) SocialThumbnailCostUSD(string, string) float64 { return f.cost }
func (f *fakeSocialProvider) SupportsSocialThumbnail(string, string) bool {
	return f.supported
}

func TestSocialThumbnailBackfillUsesStoredFirstStillForDuplicateGroup(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	original := encodeBackfillPNG(t, 173, 311, color.RGBA{220, 30, 80, 255})
	archive := backfillGalleryZIP(t, "001.png", original, false)

	first := seedSocialBackfillItem(t, db, store, "one01", "https://www.instagram.com/p/POST/", "https://www.instagram.com/p/POST/", utils.ArchiveTypeGalleryDl, archive)
	second := seedSocialBackfillItem(t, db, store, "two02", "https://instagram.com/p/POST/?utm_source=x", "https://www.instagram.com/p/POST/", utils.ArchiveTypeGalleryDl, archive)
	bad := encodeBackfillPNG(t, thumbnail.Width, thumbnail.Height, color.RGBA{30, 30, 30, 255})
	for _, item := range []*models.ArchiveItem{&first, &second} {
		key := "legacy/" + item.StorageKey + "-thumb.png"
		writeMemoryObject(t, store, key, bad)
		if err := db.Model(item).Updates(map[string]interface{}{
			"thumbnail_key": key, "thumbnail_width": thumbnail.Width,
			"thumbnail_height": thumbnail.Height, "thumbnail_status": models.ThumbnailStatusReady,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	native := &fakeSocialRefresher{err: errors.New("native must not run")}
	w := NewSocialThumbnailBackfillWorker(store, db, native, nil)
	err := w.generate(context.Background(), SocialThumbnailBackfillJobArgs{
		Identity: "https://www.instagram.com/p/POST/", URL: "https://www.instagram.com/p/POST/",
		ShortID: "two02", Type: utils.ArchiveTypeGalleryDl,
	}, false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if native.calls != 0 {
		t.Fatalf("native refresher called %d times; stored still should win", native.calls)
	}

	var got []models.ArchiveItem
	if err := db.Where("id IN ?", []uint{first.ID, second.ID}).Order("id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ThumbnailKey == "" || got[0].ThumbnailKey != got[1].ThumbnailKey {
		t.Fatalf("duplicates did not share one corrected object: %+v", got)
	}
	for _, item := range got {
		if item.ThumbnailKind != models.ThumbnailKindSocialPreview || item.ThumbnailWidth != 173 || item.ThumbnailHeight != 311 {
			t.Errorf("item %d thumbnail = kind %q %dx%d", item.ID, item.ThumbnailKind, item.ThumbnailWidth, item.ThumbnailHeight)
		}
	}
	r, err := store.Reader(got[0].ThumbnailKey)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(stored, original) {
		t.Error("stored gallery image was cropped or re-encoded")
	}
}

func TestSocialThumbnailBackfillProviderRescuesAllVideoGallery(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	videoBundle := backfillGalleryZIP(t, "001.mp4", []byte("video"), true)
	item := seedSocialBackfillItem(t, db, store, "vid01", "https://www.instagram.com/p/VIDEO/", "https://www.instagram.com/p/VIDEO/", utils.ArchiveTypeGalleryDl, videoBundle)
	if err := db.Model(&item).Update("source", models.ArchiveSourceBrightData).Error; err != nil {
		t.Fatal(err)
	}
	poster := encodeBackfillPNG(t, 720, 1280, color.RGBA{1, 80, 220, 255})
	native := &fakeSocialRefresher{err: errors.New("native refused")}
	provider := &fakeSocialProvider{thumb: &archivers.Thumbnail{Data: poster, Width: 720, Height: 1280}, cost: 0.0015, supported: true}
	w := NewSocialThumbnailBackfillWorker(store, db, native, provider)
	if err := w.generate(context.Background(), SocialThumbnailBackfillJobArgs{
		Identity: "https://www.instagram.com/p/VIDEO/", URL: "https://www.instagram.com/p/VIDEO/",
		ShortID: "vid01", Type: utils.ArchiveTypeGalleryDl,
		StartedAt: time.Now().Add(-time.Minute).Unix(), BudgetUSD: 5,
	}, false); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := reload(t, db, item.ID)
	if got.ThumbnailKind != models.ThumbnailKindSocialPreview || got.ThumbnailWidth != 270 || got.ThumbnailHeight != 480 {
		t.Fatalf("provider poster not stored: %+v", got)
	}
	if native.calls != 0 || provider.calls != 1 {
		t.Fatalf("calls native=%d provider=%d, want the successful archive provider first", native.calls, provider.calls)
	}
}

func TestSocialThumbnailBackfillRetainsLegacyPreviewWhenPosterUnavailable(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	item := seedSocialBackfillItem(t, db, store, "gone1", "https://x.com/user/status/123", "https://x.com/user/status/123", utils.ArchiveTypeGalleryDl, backfillGalleryZIP(t, "001.mp4", []byte("video"), true))
	legacyKey := "gone1/legacy-thumb.jpg"
	writeMemoryObject(t, store, legacyKey, encodeBackfillPNG(t, 480, 270, color.Black))
	if err := db.Model(&item).Updates(map[string]interface{}{
		"thumbnail_key": legacyKey, "thumbnail_width": 480, "thumbnail_height": 270,
		"thumbnail_status": models.ThumbnailStatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	native := &fakeSocialRefresher{err: errors.New("post deleted")}
	provider := &fakeSocialProvider{err: archivers.ErrSocialThumbnailUnavailable, supported: true}
	w := NewSocialThumbnailBackfillWorker(store, db, native, provider)
	if err := w.generate(context.Background(), SocialThumbnailBackfillJobArgs{
		Identity: "https://x.com/user/status/123", URL: "https://x.com/user/status/123",
		ShortID: "gone1", Type: utils.ArchiveTypeGalleryDl,
	}, true); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := reload(t, db, item.ID)
	if got.ThumbnailKey != legacyKey || got.ThumbnailStatus != models.ThumbnailStatusReady || got.ThumbnailKind != models.ThumbnailKindSocialFallback {
		t.Fatalf("legacy fallback was not retained: %+v", got)
	}
}

func TestSocialThumbnailBackfillSkipsPaidProviderAndUsesStoredVideo(t *testing.T) {
	db := newWorkerTestDB(t)
	if err := db.AutoMigrate(&models.FallbackUsage{}); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStorage()
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	item := seedSocialBackfillItem(t, db, store, "cap01", "https://www.instagram.com/reel/CAP/", "https://www.instagram.com/reel/CAP/", utils.ArchiveTypeYtDlp, video)
	native := &fakeSocialRefresher{err: errors.New("native refused")}
	provider := &fakeSocialProvider{cost: 0.01, supported: true}
	w := NewSocialThumbnailBackfillWorker(store, db, native, provider)
	if err = w.generate(context.Background(), SocialThumbnailBackfillJobArgs{
		Identity: "https://www.instagram.com/reel/CAP/", URL: "https://www.instagram.com/reel/CAP/",
		ShortID: "cap01", Type: utils.ArchiveTypeYtDlp,
		StartedAt: time.Now().Add(-time.Minute).Unix(), BudgetUSD: 0.005,
	}, true); err != nil {
		t.Fatalf("budget exhaustion should leave resumable work, got %v", err)
	}
	if provider.calls != 0 {
		t.Fatal("provider called despite insufficient budget")
	}
	if got := reload(t, db, item.ID); got.ThumbnailKind != models.ThumbnailKindSocialPreview ||
		got.ThumbnailStatus != models.ThumbnailStatusReady || got.ThumbnailWidth <= 0 || got.ThumbnailHeight <= 0 ||
		got.ThumbnailWidth > thumbnail.SocialMaxDimension || got.ThumbnailHeight > thumbnail.SocialMaxDimension {
		t.Fatalf("stored-video preview not produced after provider budget skip: %+v", got)
	}
}

func TestSocialThumbnailBackfillDryRunGroupsCanonicalDuplicates(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	canonical := "https://www.youtube.com/watch?v=abcdefghijk"
	seedSocialBackfillItem(t, db, store, "grp01", canonical, canonical, utils.ArchiveTypeYtDlp, []byte("video"))
	seedSocialBackfillItem(t, db, store, "grp02", canonical+"&feature=share", canonical, utils.ArchiveTypeYtDlp, []byte("video"))
	seedSocialBackfillItem(t, db, store, "grp03", canonical+"&archive=gallery", canonical, utils.ArchiveTypeGalleryDl, backfillGalleryZIP(t, "001.png", encodeBackfillPNG(t, 2, 3, color.White), false))

	summary, err := EnqueueSocialThumbnailBackfill(context.Background(), db, nil, SocialThumbnailBackfillOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TargetItems != 3 || summary.TargetGroups != 2 || summary.Enqueued != 0 {
		t.Fatalf("summary = %+v, want 3 items in 2 groups", summary)
	}
}

func seedSocialBackfillItem(t *testing.T, db *gorm.DB, store *storage.MemoryStorage, shortID, original, canonical, archiveType string, artifact []byte) models.ArchiveItem {
	t.Helper()
	url := models.ArchivedURL{Original: original, CanonicalURL: canonical}
	if err := db.Create(&url).Error; err != nil {
		t.Fatal(err)
	}
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatal(err)
	}
	key := shortID + "/" + archiveType + "-stored"
	writeMemoryObject(t, store, key, artifact)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: archiveType, Status: "completed", StorageKey: key}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	return item
}

func backfillGalleryZIP(t *testing.T, name string, data []byte, video bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	meta := archivers.GalleryMetadata{Files: []archivers.GalleryFile{{
		Name: name, Size: int64(len(data)), IsVideo: video,
		ContentType: map[bool]string{true: "video/mp4", false: "image/png"}[video],
	}}}
	raw, _ := json.Marshal(meta)
	mw, _ := zw.Create("metadata.json")
	_, _ = mw.Write(raw)
	fw, _ := zw.Create(name)
	_, _ = fw.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeBackfillPNG(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeMemoryObject(t *testing.T, store *storage.MemoryStorage, key string, data []byte) {
	t.Helper()
	w, err := store.Writer(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
