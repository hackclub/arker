package workers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/testfixtures"
	"arker/internal/utils"
)

// Social-archive contract tests for the job pipeline.
//
// These run processArchiveJob — the whole item lifecycle, storage-key
// construction, sidecar writes and the status flip — over the *real*
// archivers, with the fake extractors from internal/testfixtures standing in
// for yt-dlp and gallery-dl. Nothing here touches the network.

// newPipelineTestDB is newWorkerTestDB plus BrightDataUsage, which the cost
// half of the contract needs.
func newPipelineTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{},
		&models.ArchiveItemLog{}, &models.BrightDataUsage{}, &models.Config{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedPipelineItem creates a URL, a capture and one pending item of the given
// type, and returns the item plus the job args that drive it.
func seedPipelineItem(t *testing.T, db *gorm.DB, url, shortID, itemType string) (*models.ArchiveItem, ArchiveJobArgs) {
	t.Helper()
	archived := models.ArchivedURL{Original: url}
	if err := db.Create(&archived).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	capture := models.Capture{ArchivedURLID: archived.ID, Timestamp: time.Now(), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	item := models.ArchiveItem{CaptureID: capture.ID, Type: itemType, Status: "processing"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
	return &item, ArchiveJobArgs{CaptureID: capture.ID, ShortID: shortID, Type: itemType, URL: url}
}

// realArchivers is the production archiver map for the two social types.
func realArchivers() map[string]archivers.Archiver {
	return map[string]archivers.Archiver{
		utils.ArchiveTypeYtDlp:     &archivers.YtDlpArchiver{},
		utils.ArchiveTypeGalleryDl: &archivers.GalleryDLArchiver{},
	}
}

// TestPipelineStoresEveryArtifactBeforeCompleting is contract #1's ordering
// rule: a completed item can never point at a sidecar that was never written.
func TestPipelineStoresEveryArtifactBeforeCompleting(t *testing.T) {
	for _, name := range []string{"youtube_regular", "instagram_reel", "tiktok_video"} {
		t.Run(name, func(t *testing.T) {
			c := testfixtures.Lookup(t, name)
			testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})

			db := newPipelineTestDB(t)
			store := storage.NewMemoryStorage()
			item, args := seedPipelineItem(t, db, c.URL, "abc"+name[:2], utils.ArchiveTypeYtDlp)

			if err := processArchiveJob(context.Background(), args, item, store, db, realArchivers()); err != nil {
				t.Fatalf("processArchiveJob: %v", err)
			}

			var got models.ArchiveItem
			if err := db.First(&got, item.ID).Error; err != nil {
				t.Fatalf("reload item: %v", err)
			}
			if got.Status != "completed" {
				t.Fatalf("status = %q, want completed", got.Status)
			}
			for label, key := range map[string]string{
				"storage_key":      got.StorageKey,
				"metadata_key":     got.MetadataKey,
				"raw_metadata_key": got.RawMetadataKey,
			} {
				if key == "" {
					t.Fatalf("%s is empty on a completed social item", label)
				}
				exists, err := store.Exists(key)
				if err != nil || !exists {
					t.Errorf("%s points at %q, which is not in storage", label, key)
				}
			}
			// Keys share one nonce-suffixed base so the write-once bucket is
			// never asked to overwrite.
			if !strings.HasPrefix(got.MetadataKey, args.ShortID+"/") {
				t.Errorf("metadata key %q is not under the capture's prefix", got.MetadataKey)
			}
			if got.FileSize <= 0 {
				t.Error("file_size was not recorded")
			}
			if got.Source != models.ArchiveSourceNative {
				t.Errorf("source = %q, want native", got.Source)
			}
		})
	}
}

// A gallery capture stores its metadata inside the ZIP, so it has no metadata
// sidecar keys — but it must still complete with a stored bundle.
func TestPipelineStoresGalleryBundle(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})

	db := newPipelineTestDB(t)
	store := storage.NewMemoryStorage()
	item, args := seedPipelineItem(t, db, c.URL, "gal01", utils.ArchiveTypeGalleryDl)

	if err := processArchiveJob(context.Background(), args, item, store, db, realArchivers()); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey == "" {
		t.Fatalf("gallery item was not completed with a bundle: %+v", got)
	}
	if got.Extension != ".zip" {
		t.Errorf("extension = %q, want .zip", got.Extension)
	}
	if got.FileSize <= 0 {
		t.Error("bundle size was not recorded")
	}
}

// TestPipelineLeavesItemUnfinishedWhenTheExtractorRefuses is contract #1 from
// the other side: a refused extraction must not produce a stored artifact.
func TestPipelineLeavesItemUnfinishedWhenTheExtractorRefuses(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_reel")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{
		Fixture:   c.Name,
		FailProbe: true,
		Stderr:    "ERROR: [Instagram] You need to log in to access this content",
	})

	db := newPipelineTestDB(t)
	store := storage.NewMemoryStorage()
	item, args := seedPipelineItem(t, db, c.URL, "fail1", utils.ArchiveTypeYtDlp)

	err := processArchiveJob(context.Background(), args, item, store, db, realArchivers())
	if err == nil {
		t.Fatal("a refused extraction must return an error")
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status == "completed" {
		t.Fatal("a refused extraction must never leave the item completed")
	}
	if got.StorageKey != "" {
		t.Errorf("storage_key = %q, want empty for a failed run", got.StorageKey)
	}
}

// usageRecordingBackend is a mock Bright Data backend. It never talks to
// Bright Data; it writes the same usage row shape the real client writes, so
// the accounting contract can be tested without spending money.
type usageRecordingBackend struct {
	supports bool
	fail     bool
	product  string
	costUSD  float64
	records  int
	called   bool
	mu       sync.Mutex
}

func (b *usageRecordingBackend) Enabled() bool { return true }

func (b *usageRecordingBackend) SupportsFallback(url, itemType string) bool { return b.supports }

func (b *usageRecordingBackend) ArchiveFallback(ctx context.Context, url, itemType string,
	logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	b.mu.Lock()
	b.called = true
	b.mu.Unlock()

	product := b.product
	if product == "" {
		product = "web_scraper"
	}
	// A billable attempt is recorded whether or not it succeeds: a failed
	// dataset trigger still costs money, and silent spend is exactly what
	// this table exists to prevent.
	usage := &models.BrightDataUsage{
		ArchiveItemID: itemID,
		URL:           url,
		Product:       product,
		DatasetID:     "gd_fixture_dataset",
		SnapshotID:    "s_fixture_snapshot",
		Records:       b.records,
		CostUSD:       b.costUSD,
		Success:       !b.fail,
	}
	if b.fail {
		usage.Detail = "fixture: dataset returned no media"
		if db != nil {
			db.Save(usage)
		}
		fmt.Fprintf(logWriter, "fixture Bright Data backend failed\n")
		return archivers.Result{}, errors.New("bright data fixture failure")
	}
	usage.Detail = "fixture: video 512 bytes"
	if db != nil {
		db.Save(usage)
	}
	return archivers.Result{
		Data:        strings.NewReader("brightdata-rescued-media"),
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Source:      models.ArchiveSourceBrightData,
		Metadata: &archivers.Sidecar{Data: []byte(
			`{"schema_version":"1","post_id":"DbktPO1Eopi","title":"Rescued reel","provenance":"fallback","provider":"brightdata"}`)},
		RawMetadata: &archivers.Sidecar{Data: []byte(`{"provider":"brightdata"}`)},
	}, nil
}

// fallbackArchivers wraps the real yt-dlp archiver with the fallback, exactly
// as cmd/main.go does.
func fallbackArchivers(backend *usageRecordingBackend) map[string]archivers.Archiver {
	return map[string]archivers.Archiver{
		utils.ArchiveTypeYtDlp: &fallbackWrapper{
			primary: &archivers.YtDlpArchiver{},
			typ:     utils.ArchiveTypeYtDlp,
			backend: backend,
		},
	}
}

// fallbackWrapper mirrors brightdata.FallbackArchiver's decision ladder.
// It is duplicated here rather than imported because internal/brightdata
// imports internal/archivers, and the workers package needs both plus a mock;
// the ladder itself is asserted directly in internal/brightdata.
type fallbackWrapper struct {
	primary archivers.Archiver
	typ     string
	backend *usageRecordingBackend
}

func (f *fallbackWrapper) Archive(ctx context.Context, url string, logWriter io.Writer,
	db *gorm.DB, itemID uint) (archivers.Result, error) {
	result, nativeErr := f.primary.Archive(ctx, url, logWriter, db, itemID)
	if nativeErr == nil {
		return result, nil
	}
	if ctx.Err() != nil || !f.backend.SupportsFallback(url, f.typ) {
		return result, nativeErr
	}
	fallbackResult, fallbackErr := f.backend.ArchiveFallback(ctx, url, f.typ, logWriter, db, itemID)
	if fallbackErr != nil {
		return result, fmt.Errorf("native flow failed (%v); Bright Data fallback failed: %w", nativeErr, fallbackErr)
	}
	return fallbackResult, nil
}

// TestPaidFallbackIsOnlyReachedAfterAFreeFailure is contract #3: free first,
// always. The paid path must not be touched when the native run works.
func TestPaidFallbackIsOnlyReachedAfterAFreeFailure(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_reel")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})

	backend := &usageRecordingBackend{supports: true}
	db := newPipelineTestDB(t)
	store := storage.NewMemoryStorage()
	item, args := seedPipelineItem(t, db, c.URL, "free1", utils.ArchiveTypeYtDlp)

	if err := processArchiveJob(context.Background(), args, item, store, db, fallbackArchivers(backend)); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	if backend.called {
		t.Error("the paid backend was consulted even though the free path succeeded")
	}
	var usageCount int64
	db.Model(&models.BrightDataUsage{}).Count(&usageCount)
	if usageCount != 0 {
		t.Errorf("recorded %d usage rows for a free success, want 0", usageCount)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Source != models.ArchiveSourceNative {
		t.Errorf("source = %q, want native", got.Source)
	}
}

// TestPaidFallbackRescueIsRecordedAsSuchCovers contract #3's provenance rule:
// a rescued artifact is not the same thing as a native one, and an audit has
// to be able to find it without reading logs.
func TestPaidFallbackRescueIsRecordedAsSuch(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_reel")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name, FailProbe: true})

	backend := &usageRecordingBackend{supports: true, costUSD: 0.0045, records: 3}
	db := newPipelineTestDB(t)
	store := storage.NewMemoryStorage()
	item, args := seedPipelineItem(t, db, c.URL, "resc1", utils.ArchiveTypeYtDlp)

	if err := processArchiveJob(context.Background(), args, item, store, db, fallbackArchivers(backend)); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed after a successful rescue", got.Status)
	}
	if got.Source != models.ArchiveSourceBrightData {
		t.Errorf("source = %q, want brightdata", got.Source)
	}

	var usage models.BrightDataUsage
	if err := db.Where("archive_item_id = ?", item.ID).First(&usage).Error; err != nil {
		t.Fatalf("no usage row was recorded for a paid rescue: %v", err)
	}
	if !usage.Success {
		t.Error("usage row for a successful rescue has success = false")
	}
	if usage.CostUSD != 0.0045 {
		t.Errorf("cost_usd = %v, want 0.0045", usage.CostUSD)
	}
}

// TestPaidFallbackFailureLeavesItemFailedAndStillBills is contract #3's
// hardest requirement: a failed paid attempt is still a billable event.
// Recording it only on success is how spend goes invisible.
func TestPaidFallbackFailureLeavesItemFailedAndStillBills(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_reel")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name, FailProbe: true})

	backend := &usageRecordingBackend{supports: true, fail: true, costUSD: 0.0015, records: 1}
	db := newPipelineTestDB(t)
	store := storage.NewMemoryStorage()
	item, args := seedPipelineItem(t, db, c.URL, "paid1", utils.ArchiveTypeYtDlp)

	err := processArchiveJob(context.Background(), args, item, store, db, fallbackArchivers(backend))
	if err == nil {
		t.Fatal("a failed paid rescue must return an error")
	}
	if !strings.Contains(err.Error(), "Bright Data fallback failed") {
		t.Errorf("error = %q, want it to name both the native and the paid failure", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status == "completed" {
		t.Fatal("a failed paid rescue must never leave the item completed")
	}
	if got.StorageKey != "" {
		t.Error("a failed paid rescue must not leave a stored artifact")
	}

	var usage models.BrightDataUsage
	if err := db.Where("archive_item_id = ?", item.ID).First(&usage).Error; err != nil {
		t.Fatalf("a failed paid attempt recorded no usage row; spend would be invisible: %v", err)
	}
	if usage.Success {
		t.Error("usage row for a failed attempt has success = true")
	}
	if usage.CostUSD != 0.0015 {
		t.Errorf("cost_usd = %v, want the attempt to still be billed", usage.CostUSD)
	}
	if usage.Detail == "" {
		t.Error("a failed usage row carries no detail explaining what was paid for")
	}
}

// TestFindOrCreateCreatesExactlyOneCaptureUnderConcurrency is contract #2's
// concurrency-safety rule.
//
// Production serializes this with pg_advisory_xact_lock on the URL
// (queue.go:121-125). That lock is a no-op on SQLite by design, so asserting
// it means asserting against Postgres — the same gate the repo already uses
// for its other Postgres-only test, and one CI satisfies.
func TestFindOrCreateCreatesExactlyOneCaptureUnderConcurrency(t *testing.T) {
	db, cleanup := postgresTestDB(t)
	defer cleanup()

	const url = "https://www.youtube.com/watch?v=aqz-KE-bpKQ"
	types := []string{utils.ArchiveTypeYtDlp}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]FindOrCreateResult, racers)
	errs := make([]error, racers)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = FindOrCreateCapture(context.Background(), db, nil, url, types, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	shortIDs := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		shortIDs[results[i].ShortID] = true
		if results[i].Action == FindOrCreateCreated {
			created++
		}
	}

	if created != 1 {
		t.Errorf("%d racers reported creating a capture, want exactly 1", created)
	}
	if len(shortIDs) != 1 {
		t.Errorf("racers returned %d distinct short IDs, want 1: %v", len(shortIDs), shortIDs)
	}

	var captures int64
	db.Model(&models.Capture{}).Count(&captures)
	if captures != 1 {
		t.Errorf("%d captures exist for one URL, want 1", captures)
	}
	var urls int64
	db.Model(&models.ArchivedURL{}).Count(&urls)
	if urls != 1 {
		t.Errorf("%d archived_urls rows exist for one URL, want 1", urls)
	}
}

// TestFindOrCreateJoinsRatherThanDuplicatesWhenWorkIsInFlight is the same rule
// on the serialized path, so it runs everywhere rather than only under
// Postgres.
func TestFindOrCreateJoinsRatherThanDuplicatesWhenWorkIsInFlight(t *testing.T) {
	db := newPipelineTestDB(t)
	const url = "https://www.instagram.com/p/DbktPO1Eopi/"
	types := []string{utils.ArchiveTypeGalleryDl}

	first, err := FindOrCreateCapture(context.Background(), db, nil, url, types, nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first.Action != FindOrCreateCreated {
		t.Fatalf("first call action = %q, want created", first.Action)
	}

	second, err := FindOrCreateCapture(context.Background(), db, nil, url, types, nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Action != FindOrCreateInProgress {
		t.Errorf("second call action = %q, want in_progress", second.Action)
	}
	if second.ShortID != first.ShortID {
		t.Errorf("second call joined %q, want the in-flight capture %q", second.ShortID, first.ShortID)
	}

	var captures int64
	db.Model(&models.Capture{}).Count(&captures)
	if captures != 1 {
		t.Errorf("%d captures exist, want 1", captures)
	}
}

// TestFindOrCreateReusesACompletedCaptureOfAnyAge pins contract #2's "newest
// completed qualifying archive regardless of age" — there is deliberately no
// freshness window on this path, unlike aliasing.
func TestFindOrCreateReusesACompletedCaptureOfAnyAge(t *testing.T) {
	db := newPipelineTestDB(t)
	const url = "https://bsky.app/profile/bsky.app/post/3msqpuobiwk2t"
	types := []string{utils.ArchiveTypeGalleryDl}

	old := seedCapture(t, db, url, "oldy1", 400*24*time.Hour,
		map[string]string{utils.ArchiveTypeGalleryDl: "completed"})
	setItemUpdatedAt(t, db, old.ShortID, utils.ArchiveTypeGalleryDl, time.Now().Add(-400*24*time.Hour))

	got, err := FindOrCreateCapture(context.Background(), db, nil, url, types, nil)
	if err != nil {
		t.Fatalf("FindOrCreateCapture: %v", err)
	}
	if got.Action != FindOrCreateFound {
		t.Errorf("action = %q, want found: a completed archive is reusable at any age", got.Action)
	}
	if got.ShortID != old.ShortID {
		t.Errorf("short id = %q, want the year-old capture %q", got.ShortID, old.ShortID)
	}
}
