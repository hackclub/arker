package workers

import (
	"context"
	"strings"
	"testing"

	"arker/internal/models"
	"arker/internal/storage"
)

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
