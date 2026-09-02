package workers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

type leanMetadataRefresher struct {
	normalCalls int
	leanCalls   int
}

type failedLeanMetadataRefresher struct{ calls int }

func (r *failedLeanMetadataRefresher) RefreshVideoMetadata(context.Context, string, io.Writer, archivers.VideoMedia) (archivers.Result, error) {
	r.calls++
	return archivers.Result{}, errors.New("native refused")
}

func (r *failedLeanMetadataRefresher) RefreshVideoContractMetadata(context.Context, string, io.Writer, archivers.VideoMedia) (archivers.Result, error) {
	r.calls++
	return archivers.Result{}, errors.New("native refused")
}

type providerMetadataRefresher struct {
	calls int
	media archivers.VideoMedia
}

type failedProviderMetadataRefresher struct{ calls int }

func (r *failedProviderMetadataRefresher) SupportsStoredVideoMetadata(string) bool { return true }

func (r *failedProviderMetadataRefresher) RefreshStoredVideoMetadata(context.Context, string, io.Writer, *gorm.DB, uint, archivers.VideoMedia) (archivers.Result, error) {
	r.calls++
	return archivers.Result{}, errors.New("provider no longer sees post")
}

func (r *providerMetadataRefresher) SupportsStoredVideoMetadata(string) bool { return true }

func (r *providerMetadataRefresher) RefreshStoredVideoMetadata(_ context.Context, _ string, _ io.Writer, _ *gorm.DB, _ uint, media archivers.VideoMedia) (archivers.Result, error) {
	r.calls++
	r.media = media
	return archivers.Result{
		Extension: ".mp4", ContentType: "video/mp4",
		Metadata:     &archivers.Sidecar{Data: []byte(`{"schema_version":"1","title":"Provider rescue"}`)},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"id":"provider"}`)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

func (r *leanMetadataRefresher) RefreshVideoMetadata(context.Context, string, io.Writer, archivers.VideoMedia) (archivers.Result, error) {
	r.normalCalls++
	return archivers.Result{}, nil
}

func (r *leanMetadataRefresher) RefreshVideoContractMetadata(context.Context, string, io.Writer, archivers.VideoMedia) (archivers.Result, error) {
	r.leanCalls++
	return archivers.Result{
		Extension: ".mp4", ContentType: "video/mp4",
		Metadata:     &archivers.Sidecar{Data: []byte(`{"schema_version":"1","title":"Lean"}`)},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"id":"lean"}`)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

func TestVideoMetadataBackfillRefreshesWithoutDownloadingMedia(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	item := seedPriorVideoCapture(t, db, store, url, "oldmd", func(item *models.ArchiveItem) {
		item.MetadataKey = ""
		item.RawMetadataKey = ""
		item.Completeness = ""
	})
	refresher := &refreshTrackingArchiver{}
	worker := NewVideoMetadataBackfillWorker(store, db, refresher)
	if err := worker.generate(context.Background(), VideoMetadataBackfillJobArgs{
		Identity: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", URL: url, ShortID: "oldmd", Version: 1,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refresher.refreshCalls != 1 || refresher.archiveCalls != 0 {
		t.Fatalf("refresh/full calls = %d/%d, want 1/0", refresher.refreshCalls, refresher.archiveCalls)
	}
	if got.MetadataKey == "" || got.RawMetadataKey == "" || !strings.Contains(got.MetadataKey, "metadata-backfill") {
		t.Fatalf("backfilled sidecars = %q / %q", got.MetadataKey, got.RawMetadataKey)
	}
	if got.StorageKey != item.StorageKey {
		t.Fatalf("media changed from %q to %q", item.StorageKey, got.StorageKey)
	}
}

func TestVideoMetadataBackfillDryRunGroupsDuplicates(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	seedPriorVideoCapture(t, db, store, url, "oldm1", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	item := newPendingVideoItem(t, db, url, "oldm2")
	db.Model(&item).Updates(map[string]interface{}{"status": "completed", "storage_key": "oldm1/yt-dlp-aaaa1111.mp4"})
	summary, err := EnqueueVideoMetadataBackfill(context.Background(), db, nil, VideoMetadataBackfillOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TargetItems != 2 || summary.TargetGroups != 1 || summary.Enqueued != 0 {
		t.Fatalf("summary = %+v, want 2 rows in one canonical group", summary)
	}
}

func TestVideoMetadataBackfillPrefersLeanContractRefresh(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	item := seedPriorVideoCapture(t, db, store, url, "lean1", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	refresher := &leanMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, refresher)
	if err := worker.generate(context.Background(), VideoMetadataBackfillJobArgs{
		Identity: url, URL: url, ShortID: "lean1", Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if refresher.leanCalls != 1 || refresher.normalCalls != 0 {
		t.Fatalf("lean/normal calls = %d/%d, want 1/0", refresher.leanCalls, refresher.normalCalls)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.MetadataKey == "" || got.RawMetadataKey == "" {
		t.Fatalf("lean refresh did not persist both sidecars: %+v", got)
	}
}

func TestVideoMetadataBackfillUsesMetadataOnlyProviderOnFinalAttempt(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.instagram.com/reel/metadata-rescue/"
	item := seedPriorVideoCapture(t, db, store, url, "rescu", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	primary := &failedLeanMetadataRefresher{}
	provider := &providerMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, primary, provider)
	args := VideoMetadataBackfillJobArgs{Identity: url, URL: url, ShortID: "rescu", Version: 2}

	if err := worker.generateAttempt(context.Background(), args, false); err == nil {
		t.Fatal("first native failure should remain retryable")
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls after non-final attempt = %d, want 0", provider.calls)
	}
	if err := worker.generateAttempt(context.Background(), args, true); err != nil {
		t.Fatalf("final provider rescue: %v", err)
	}
	if primary.calls != 1 || provider.calls != 1 {
		t.Fatalf("native/provider calls = %d/%d, want 1/1", primary.calls, provider.calls)
	}
	if provider.media.Extension != item.Extension || provider.media.SizeBytes != item.FileSize {
		t.Errorf("provider media = %+v, want stored extension %q and size %d", provider.media, item.Extension, item.FileSize)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.MetadataKey == "" || got.RawMetadataKey == "" {
		t.Fatalf("provider rescue did not persist both sidecars: %+v", got)
	}
}

func TestVideoMetadataBackfillCanExplicitlyPreferMetadataOnlyProvider(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://vimeo.com/1190579612"
	item := seedPriorVideoCapture(t, db, store, url, "vimeo", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	provider := &providerMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, nil, provider)
	args := VideoMetadataBackfillJobArgs{Identity: url, URL: url, ShortID: "vimeo", Version: 4, ProviderFirst: true}
	if err := worker.generateAttempt(context.Background(), args, false); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.MetadataKey == "" || got.RawMetadataKey == "" {
		t.Fatalf("provider-first refresh did not persist both sidecars: %+v", got)
	}
}

func TestVideoMetadataBackfillPrefersCapturedMHTMLToLiveOrPaidProvider(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://youtube.com/shorts/jAUZFBlZmiE"
	item := seedPriorVideoCapture(t, db, store, url, "mhtml", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	mhtmlKey := "mhtml/page.mhtml"
	mhtml := "Content-Type: multipart/related; boundary=root\r\n\r\n--root\r\nContent-Type: text/html\r\n\r\n" +
		`<html><head><meta property="og:title" content="Captured title"><meta itemprop="datePublished" content="2026-07-15T05:55:09-07:00"><meta itemprop="duration" content="PT33S"><span itemprop="author"><link itemprop="name" content="Captured channel"></span></head></html>` +
		"\r\n--root--\r\n"
	putObject(t, store, mhtmlKey, []byte(mhtml))
	if err := db.Create(&models.ArchiveItem{CaptureID: item.CaptureID, Type: "mhtml", Status: "completed", StorageKey: mhtmlKey, Extension: ".mhtml"}).Error; err != nil {
		t.Fatal(err)
	}
	primary := &failedLeanMetadataRefresher{}
	provider := &providerMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, primary, provider)
	args := VideoMetadataBackfillJobArgs{Identity: utils.CanonicalizeArchiveURL(url), URL: url, ShortID: "mhtml", Version: 2}
	if err := worker.generateAttempt(context.Background(), args, false); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatalf("paid provider calls = %d, want 0 when captured MHTML is usable", provider.calls)
	}
	if primary.calls != 0 {
		t.Fatalf("second live extractor calls = %d, want 0 when captured MHTML is usable", primary.calls)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := store.Reader(got.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var metadata archivers.VideoMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Captured title" || metadata.Channel != "Captured channel" || metadata.Provider != "captured_mhtml" {
		t.Errorf("captured metadata = %+v", metadata)
	}
}

func TestVideoMetadataBackfillUsesArchivedProbeLogAfterProviderFailure(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://www.instagram.com/reel/deleted-post/"
	item := seedPriorVideoCapture(t, db, store, url, "logmd", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "Testing video accessibility with yt-dlp...\nVideo info:\nCaptured reel title\n12.75\nCaptured author\n\nStarting yt-dlp download process...\n"); err != nil {
		t.Fatal(err)
	}
	primary := &failedLeanMetadataRefresher{}
	provider := &failedProviderMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, primary, provider)
	args := VideoMetadataBackfillJobArgs{Identity: utils.CanonicalizeArchiveURL(url), URL: url, ShortID: "logmd", Version: 2}
	if err := worker.generateAttempt(context.Background(), args, true); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 0 || provider.calls != 1 {
		t.Fatalf("native/provider calls = %d/%d, want 0/1", primary.calls, provider.calls)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := store.Reader(got.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var metadata archivers.VideoMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Captured reel title" || metadata.Channel != "Captured author" || metadata.DurationSeconds == nil || *metadata.DurationSeconds != 12.75 || metadata.Provider != "captured_probe_log" {
		t.Fatalf("captured probe metadata = %+v", metadata)
	}
}

func TestVideoMetadataBackfillUsesCanonicalVimeoPageAfterProviderFailure(t *testing.T) {
	db := newWorkerTestDB(t)
	store := storage.NewMemoryStorage()
	url := "https://vimeo.com/770625302"
	item := seedPriorVideoCapture(t, db, store, url, "vimmd", func(item *models.ArchiveItem) {
		item.MetadataKey, item.RawMetadataKey = "", ""
	})
	mhtmlKey := "vimmd/page.mhtml"
	mhtml := "Content-Type: multipart/related; boundary=root\r\n\r\n--root\r\nContent-Type: text/html\r\n\r\n" +
		`<html><head><meta property="og:title" content="Cedric Hutchings - Sprig"><meta property="og:description" content="The console where every player is creator."><link rel="canonical" href="https://vimeo.com/770625302"></head></html>` +
		"\r\n--root--\r\n"
	putObject(t, store, mhtmlKey, []byte(mhtml))
	if err := db.Create(&models.ArchiveItem{CaptureID: item.CaptureID, Type: "mhtml", Status: "completed", StorageKey: mhtmlKey, Extension: ".mhtml"}).Error; err != nil {
		t.Fatal(err)
	}
	primary := &failedLeanMetadataRefresher{}
	provider := &failedProviderMetadataRefresher{}
	worker := NewVideoMetadataBackfillWorker(store, db, primary, provider)
	args := VideoMetadataBackfillJobArgs{Identity: utils.CanonicalizeArchiveURL(url), URL: url, ShortID: "vimmd", Version: 3}
	if err := worker.generateAttempt(context.Background(), args, true); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 0 || provider.calls != 1 {
		t.Fatalf("native/provider calls = %d/%d, want 0/1", primary.calls, provider.calls)
	}
	var got models.ArchiveItem
	if err := db.First(&got, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := store.Reader(got.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var metadata archivers.VideoMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Cedric Hutchings - Sprig" || metadata.Description == "" || metadata.Provider != "captured_mhtml" {
		t.Fatalf("canonical Vimeo metadata = %+v", metadata)
	}
}
