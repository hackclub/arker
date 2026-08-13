package canary

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"arker/internal/workers"
)

// newTestRunner wires a Runner against in-memory storage and SQLite, with the
// archive step stubbed. No extractor, browser, or network is involved, and the
// stub is handed the archiver map the runner selected, so tests can assert
// which map a probe would have run on.
func newTestRunner(t *testing.T, db *gorm.DB, store storage.Storage, cfg Config, catalog []Probe, archive ArchiveFunc) *Runner {
	t.Helper()
	return New(Options{
		DB:                db,
		Store:             store,
		NativeArchivers:   map[string]archivers.Archiver{utils.ArchiveTypeYtDlp: nativeStub{}, utils.ArchiveTypeGalleryDl: nativeStub{}},
		Config:            cfg,
		Catalog:           catalog,
		ArchiveFn:         archive,
		CookiesConfigured: func() bool { return false },
	})
}

// completingArchive simulates a healthy yt-dlp run: it writes the artifact and
// both sidecars, then flips the item to completed exactly as
// saveArchiveResult does.
func completingArchive(t *testing.T, db *gorm.DB, store storage.Storage) ArchiveFunc {
	t.Helper()
	return func(_ context.Context, args workers.ArchiveJobArgs, item *models.ArchiveItem, _ map[string]archivers.Archiver) error {
		keyBase := args.ShortID + "/yt-dlp-test"
		fixture := videoFixture(t, store, func(_ *archivers.VideoMetadata, fixtureItem *models.ArchiveItem) {
			fixtureItem.StorageKey = keyBase + ".mp4"
			fixtureItem.MetadataKey = keyBase + ".metadata.json"
			fixtureItem.RawMetadataKey = keyBase + ".raw-metadata.json"
		})
		return db.Model(&models.ArchiveItem{}).Where("id = ?", item.ID).Updates(map[string]any{
			"status":           "completed",
			"storage_key":      fixture.StorageKey,
			"extension":        ".mp4",
			"file_size":        fixture.FileSize,
			"metadata_key":     fixture.MetadataKey,
			"raw_metadata_key": fixture.RawMetadataKey,
			"source":           models.ArchiveSourceNative,
		}).Error
	}
}

func TestRunSweepRecordsPassingProbe(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	catalog := []Probe{withDefaults(videoProbe())}
	runner := newTestRunner(t, db, store, Config{}, catalog, completingArchive(t, db, store))

	result, err := runner.RunSweep(context.Background(), SweepOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if result.Passed != 1 || result.Failed != 0 {
		t.Fatalf("sweep passed=%d failed=%d, want 1/0 (%+v)", result.Passed, result.Failed, result.Results)
	}
	if result.PaidAllowed {
		t.Error("a default sweep reported paid probes as allowed")
	}

	var runs []models.CanaryRun
	if err := db.Find(&runs).Error; err != nil {
		t.Fatalf("read canary_runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d canary runs, want 1", len(runs))
	}
	run := runs[0]
	switch {
	case !run.Passed:
		t.Errorf("stored run is not marked passed: %s / %s", run.FailureStage, run.FailureReason)
	case run.StageReached != StagePassed:
		t.Errorf("stage reached = %q, want %q", run.StageReached, StagePassed)
	case run.ShortID == "":
		t.Error("stored run has no capture short ID to inspect")
	case run.FinishedAt == nil:
		t.Error("stored run has no finish time")
	case run.Provenance != models.ArchiveSourceNative:
		t.Errorf("provenance = %q, want native", run.Provenance)
	case run.CostUSD != 0:
		t.Errorf("cost = %v, want 0", run.CostUSD)
	case run.Trigger != TriggerManual:
		t.Errorf("trigger = %q, want %q", run.Trigger, TriggerManual)
	}

	// The probe archived through the real capture path, so the capture and its
	// item exist and are inspectable like any other archive.
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ?", run.ShortID).First(&item).Error; err != nil {
		t.Fatalf("probe capture %s has no archive item: %v", run.ShortID, err)
	}
	if item.Status != "completed" || item.Type != utils.ArchiveTypeYtDlp {
		t.Errorf("probe item = %s/%s, want completed yt-dlp", item.Type, item.Status)
	}

	// Only the media item is captured. Adding MHTML and screenshot would launch
	// a browser per probe every interval for no extra contract signal.
	var itemCount int64
	db.Model(&models.ArchiveItem{}).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ?", run.ShortID).Count(&itemCount)
	if itemCount != 1 {
		t.Errorf("probe capture has %d archive items, want just the media one", itemCount)
	}
}

// A native failure is recorded as a native failure, with the extractor's own
// message. This is the case where production would have escalated to a paid
// fallback; the canary reports it instead of buying its way out.
func TestRunSweepRecordsNativeFailure(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	catalog := []Probe{withDefaults(videoProbe())}
	archive := func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
		return errors.New("ERROR: unable to download video data: HTTP Error 403")
	}
	runner := newTestRunner(t, db, store, Config{}, catalog, archive)

	result, err := runner.RunSweep(context.Background(), SweepOptions{Trigger: TriggerSchedule})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("sweep failed=%d, want 1", result.Failed)
	}

	var run models.CanaryRun
	if err := db.First(&run).Error; err != nil {
		t.Fatalf("read canary_runs: %v", err)
	}
	if run.Passed {
		t.Fatal("a failed archive was recorded as a pass")
	}
	if run.FailureStage != StageArchive {
		t.Errorf("failure stage = %q, want %q", run.FailureStage, StageArchive)
	}
	if !strings.Contains(run.FailureReason, "free native path") || !strings.Contains(run.FailureReason, "403") {
		t.Errorf("failure reason %q should name the native path and carry the extractor error", run.FailureReason)
	}
	if run.Provenance != models.ArchiveSourceNative {
		t.Errorf("provenance = %q, want native", run.Provenance)
	}
}

// A probe URL that no longer routes to a media archiver fails before anything
// is downloaded, and creates no capture.
func TestRunSweepFailsRoutingWithoutArchiving(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	probe := withDefaults(videoProbe())
	probe.URL = "https://example.com/ordinary-page"
	archive := func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
		t.Fatal("archive ran despite a routing failure")
		return nil
	}
	runner := newTestRunner(t, db, store, Config{}, []Probe{probe}, archive)

	if _, err := runner.RunSweep(context.Background(), SweepOptions{}); err != nil {
		t.Fatalf("RunSweep: %v", err)
	}

	var run models.CanaryRun
	if err := db.First(&run).Error; err != nil {
		t.Fatalf("read canary_runs: %v", err)
	}
	if run.Passed || run.FailureStage != StageRouting {
		t.Fatalf("failure stage = %q (passed=%v), want %q", run.FailureStage, run.Passed, StageRouting)
	}
	var captures int64
	db.Model(&models.Capture{}).Count(&captures)
	if captures != 0 {
		t.Errorf("routing failure created %d captures, want none", captures)
	}
}

// The structural guard: a billable archiver in the native map aborts the sweep
// before a single probe runs.
func TestRunSweepAbortsWhenPaidArchiverIsInScope(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	runner := New(Options{
		DB:              db,
		Store:           store,
		NativeArchivers: map[string]archivers.Archiver{utils.ArchiveTypeYtDlp: paidStub{}},
		Config:          Config{},
		Catalog:         []Probe{withDefaults(videoProbe())},
		ArchiveFn: func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
			t.Fatal("a probe ran with a billable archiver in scope")
			return nil
		},
		CookiesConfigured: func() bool { return false },
	})

	result, err := runner.RunSweep(context.Background(), SweepOptions{})
	if err == nil {
		t.Fatal("sweep did not refuse a billable archiver")
	}
	if result.Aborted == "" {
		t.Error("aborted sweep did not report why")
	}
	var runs int64
	db.Model(&models.CanaryRun{}).Count(&runs)
	if runs != 0 {
		t.Errorf("aborted sweep recorded %d runs, want none", runs)
	}
}

// With paid probes off, probes are handed the native map. With them on and
// inside budget, they are handed the paid one.
func TestRunSweepSelectsArchiverMapByBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		wantPaid bool
	}{
		{name: "default is native-only", cfg: Config{}},
		{
			name:     "paid opt-in within budget",
			cfg:      Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
			wantPaid: true,
		},
		{
			name: "paid opt-in with no budget left",
			cfg:  Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 0.10},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			store := storage.NewMemoryStorage()
			paidMap := map[string]archivers.Archiver{utils.ArchiveTypeYtDlp: paidStub{}}
			var sawPaid bool
			runner := New(Options{
				DB:              db,
				Store:           store,
				NativeArchivers: map[string]archivers.Archiver{utils.ArchiveTypeYtDlp: nativeStub{}},
				PaidArchivers:   paidMap,
				Config:          tc.cfg,
				Catalog:         []Probe{withDefaults(videoProbe())},
				ArchiveFn: func(_ context.Context, _ workers.ArchiveJobArgs, _ *models.ArchiveItem, m map[string]archivers.Archiver) error {
					sawPaid = AssertNativeOnly(m) != nil
					return errors.New("stop here")
				},
				CookiesConfigured: func() bool { return false },
			})
			if _, err := runner.RunSweep(context.Background(), SweepOptions{}); err != nil {
				t.Fatalf("RunSweep: %v", err)
			}
			if sawPaid != tc.wantPaid {
				t.Errorf("probe ran with paid archivers = %v, want %v", sawPaid, tc.wantPaid)
			}
		})
	}
}

// Spend recorded against a probe's capture fails the probe even when the
// archive itself looks perfect.
func TestRunSweepFailsProbeThatSpentMoney(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	archive := completingArchive(t, db, store)
	billing := func(ctx context.Context, args workers.ArchiveJobArgs, item *models.ArchiveItem, m map[string]archivers.Archiver) error {
		if err := archive(ctx, args, item, m); err != nil {
			return err
		}
		return db.Create(&models.BrightDataUsage{ShortID: args.ShortID, Product: "browser_api", CostUSD: 0.02, Success: true}).Error
	}
	runner := newTestRunner(t, db, store, Config{}, []Probe{withDefaults(videoProbe())}, billing)

	if _, err := runner.RunSweep(context.Background(), SweepOptions{}); err != nil {
		t.Fatalf("RunSweep: %v", err)
	}

	var run models.CanaryRun
	if err := db.First(&run).Error; err != nil {
		t.Fatalf("read canary_runs: %v", err)
	}
	if run.Passed {
		t.Fatal("a probe that spent money was recorded as a pass")
	}
	if run.FailureStage != StageProvenance {
		t.Errorf("failure stage = %q, want %q", run.FailureStage, StageProvenance)
	}
	if run.CostUSD <= 0 {
		t.Errorf("cost = %v, want the recorded spend", run.CostUSD)
	}
}

func TestRunSweepFilterByPlatform(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	catalog := []Probe{withDefaults(videoProbe()), withDefaults(galleryProbe())}
	var archived []string
	archive := func(_ context.Context, args workers.ArchiveJobArgs, _ *models.ArchiveItem, _ map[string]archivers.Archiver) error {
		archived = append(archived, args.URL)
		return errors.New("stop here")
	}
	runner := newTestRunner(t, db, store, Config{}, catalog, archive)

	if _, err := runner.RunSweep(context.Background(), SweepOptions{Platform: "reddit"}); err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	if len(archived) != 1 || !strings.Contains(archived[0], "reddit.com") {
		t.Fatalf("archived %v, want only the reddit probe", archived)
	}
}

func TestRunSweepRefusesConcurrentSweeps(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	release := make(chan struct{})
	entered := make(chan struct{})
	archive := func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
		close(entered)
		<-release
		return errors.New("stop here")
	}
	runner := newTestRunner(t, db, store, Config{}, []Probe{withDefaults(videoProbe())}, archive)

	go func() {
		_, _ = runner.RunSweep(context.Background(), SweepOptions{})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first sweep never started")
	}

	if !runner.InProgress() {
		t.Error("InProgress is false while a sweep is running")
	}
	if _, err := runner.RunSweep(context.Background(), SweepOptions{}); !errors.Is(err, ErrSweepInProgress) {
		t.Errorf("second sweep error = %v, want ErrSweepInProgress", err)
	}
	close(release)
}

// withDefaults marks a fixture probe as part of the default set so
// SelectProbes picks it up without explicit configuration.
func withDefaults(p Probe) Probe {
	p.DefaultEnabled = true
	return p
}
