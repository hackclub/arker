package workers

import (
	"context"
	"io"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
)

type leanMetadataRefresher struct {
	normalCalls int
	leanCalls   int
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
