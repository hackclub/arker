package workers

// Reusing stored video media across captures of one URL.
//
// When a URL is archived again — a forced re-archive, or a fresh capture after
// the freshness window — the video download is the expensive and fragile half
// of the yt-dlp job, and the bytes it would fetch are the bytes we already
// hold: posts are immutable and the archive bucket is append-only, so the
// earlier object is guaranteed to still exist. What a repeat capture is
// actually for is everything around the bytes: current metadata (view counts,
// title/description edits), captions, the poster, and the browser artifacts
// (MHTML, screenshot), which run as their own jobs and are unaffected here.
//
// So a yt-dlp job whose URL already has a completed video item runs the
// extractor metadata-only, writes fresh sidecars under the new capture's key
// base, and points its storage key at the earlier media object. If even the
// metadata refresh fails (the post was deleted, the platform refuses us), the
// earlier item's sidecars are reused so the capture still completes with
// everything the archive holds.

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

// findReusableVideoItem returns the newest completed yt-dlp item whose media
// is already in storage, across every capture sharing url's canonical
// identity, or nil when the video has never been archived. The legacy
// "youtube" type spelling and rows without a backfilled canonical_url are both
// still matched, so pre-rename archives keep their videos reusable.
//
// Paid-fallback rescues (Apify, and historical Bright Data) are deliberately
// NOT reused: a fallback can be capped below native fidelity (Bright Data
// YouTube was 360p; Apify YouTube tops out at the downloader's best
// progressive rendition), so its bytes are a stand-in, not necessarily the
// bytes a fresh native run would fetch. A repeat capture is the archive's
// one chance to upgrade such a video to full fidelity once whatever blocked
// the native path (an IP flag, a cookie outage) has been fixed. If the native
// path is still blocked, the fallback simply buys the same rescue again.
func findReusableVideoItem(db *gorm.DB, url string, excludeItemID uint) *models.ArchiveItem {
	canonical := utils.CanonicalizeArchiveURL(url)
	var rows []models.ArchivedURL
	if err := db.Where("canonical_url = ? OR original = ? OR original = ?", canonical, url, canonical).
		Find(&rows).Error; err != nil {
		slog.Warn("Could not look up archived URLs for video reuse", "url", url, "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	var item models.ArchiveItem
	err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.archived_url_id IN ?", archivedURLIDs(rows)).
		Where("archive_items.type IN ?", utils.ArchiveTypeMatchValues(utils.ArchiveTypeYtDlp)).
		Where("archive_items.status = ?", "completed").
		Where("archive_items.storage_key <> ''").
		Where("(archive_items.source IS NULL OR archive_items.source NOT IN ?)", []string{models.ArchiveSourceBrightData, models.ArchiveSourceApify}).
		Where("archive_items.id <> ?", excludeItemID).
		Order("archive_items.updated_at DESC").
		First(&item).Error
	if err != nil {
		return nil
	}
	return &item
}

// refreshVideoFromStored completes a yt-dlp item by reusing prior's stored
// media and refreshing the metadata around it. It returns handled=false when
// the job should fall through to a normal full run instead: the archiver
// cannot refresh, or the refresh failed and the earlier item has no sidecars
// worth inheriting.
func refreshVideoFromStored(ctx context.Context, jobArgs ArchiveJobArgs, item, prior *models.ArchiveItem, arch archivers.Archiver, store storage.Storage, db *gorm.DB, logWriter io.Writer) (bool, error) {
	refresher, ok := arch.(archivers.VideoMetadataRefresher)
	if !ok {
		return false, nil
	}

	fmt.Fprintf(logWriter, "Video for this URL is already archived (storage key %s); skipping the download and refreshing metadata only\n", prior.StorageKey)
	slog.Info("Reusing stored video media",
		"short_id", jobArgs.ShortID,
		"url", jobArgs.URL,
		"prior_item_id", prior.ID,
		"storage_key", prior.StorageKey)

	result, err := refresher.RefreshVideoMetadata(ctx, jobArgs.URL, logWriter, archivers.VideoMedia{
		Extension: prior.Extension,
		SizeBytes: prior.FileSize,
	})
	if result.Bundle != nil {
		defer result.Bundle.Cleanup()
	}
	if err != nil {
		// The bytes are safe either way; the question is which sidecars the new
		// capture gets. A deleted post or a platform refusal must not fail a
		// capture whose product we already hold, so the earlier sidecars are
		// inherited when they exist. When they do not (a legacy row), a full run
		// is the only path to metadata, so fall through and let it try.
		if prior.MetadataKey != "" && prior.RawMetadataKey != "" {
			fmt.Fprintf(logWriter, "\nMetadata refresh failed (%v); reusing the metadata stored with the earlier archive\n", err)
			slog.Warn("Video metadata refresh failed; inheriting stored sidecars",
				"short_id", jobArgs.ShortID, "url", jobArgs.URL, "error", err)
			if updateErr := completeVideoItemFromPrior(db, item, prior); updateErr != nil {
				return true, updateErr
			}
			copyPriorThumbnail(db, item, prior)
			return true, nil
		}
		fmt.Fprintf(logWriter, "\nMetadata refresh failed (%v) and the earlier item stored no metadata; falling back to a full download\n", err)
		return false, nil
	}

	nonce := uploadNonce()
	keyBase := fmt.Sprintf("%s/%s-%s", jobArgs.ShortID, jobArgs.Type, nonce)
	if err := saveRefreshedArchiveResult(ctx, result, keyBase, prior, store, db, item, logWriter); err != nil {
		slog.Error("Failed to save refreshed video metadata",
			"short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		fmt.Fprintf(logWriter, "\nFailed to save refreshed metadata: %v\n", err)
		return true, err
	}

	slog.Info("Video archive completed from stored media",
		"short_id", jobArgs.ShortID,
		"type", jobArgs.Type,
		"storage_key", prior.StorageKey)

	// The preview is cosmetic and must never fail the item. Prefer the fresh
	// poster; fall back to sharing the earlier capture's thumbnail object.
	if result.Thumbnail != nil {
		thumbKey := fmt.Sprintf("%s-thumb%s", keyBase, thumbnail.FileExtension(result.Thumbnail.Data))
		if err := StoreThumbnail(result.Thumbnail, thumbKey, store, db, item); err != nil {
			slog.Warn("Failed to save refreshed thumbnail", "short_id", jobArgs.ShortID, "error", err)
			fmt.Fprintf(logWriter, "\nWarning: failed to save thumbnail: %v\n", err)
			copyPriorThumbnail(db, item, prior)
		}
	} else {
		copyPriorThumbnail(db, item, prior)
	}
	return true, nil
}

// saveRefreshedArchiveResult finalizes a metadata-only refresh: fresh sidecars
// and caption tracks go down under the new capture's key base, while the
// storage key, extension, and size are inherited from the stored media object.
// The stored bytes are probed exactly like a fresh download's, so intrinsic
// media facts in the refreshed record stay authoritative.
func saveRefreshedArchiveResult(ctx context.Context, result archivers.Result, keyBase string, prior *models.ArchiveItem, store storage.Storage, db *gorm.DB, item *models.ArchiveItem, logWriter io.Writer) error {
	if result.Metadata == nil || len(result.Metadata.Data) == 0 {
		return fmt.Errorf("metadata refresh produced no metadata")
	}

	extraKeys := writeExtraArtifacts(result, keyBase, store)

	metadataData := backfillStoredVideoMetadata(ctx, store, prior.StorageKey, result.Metadata.Data, logWriter)
	metadataData, err := archivers.SetSubtitleStorageKeys(metadataData, extraKeys)
	if err != nil {
		return fmt.Errorf("failed to record extra artifact keys: %w", err)
	}
	metadataKey := keyBase + ".metadata.json"
	if err := writeJSONSidecar(store, metadataKey, metadataData); err != nil {
		return fmt.Errorf("failed to store normalized metadata: %w", err)
	}
	rawMetadataKey := ""
	if result.RawMetadata != nil {
		rawMetadataKey = keyBase + ".raw-metadata.json"
		if err := writeJSONSidecar(store, rawMetadataKey, result.RawMetadata.Data); err != nil {
			return fmt.Errorf("failed to store raw metadata: %w", err)
		}
	}

	updates := map[string]interface{}{
		"status":           "completed",
		"storage_key":      prior.StorageKey,
		"extension":        prior.Extension,
		"file_size":        prior.FileSize,
		"metadata_key":     metadataKey,
		"raw_metadata_key": rawMetadataKey,
	}
	// Source describes the provenance of the stored bytes, which are the
	// earlier capture's; the refreshed sidecars do not change that.
	if prior.Source != "" {
		updates["source"] = prior.Source
	}
	if result.Completeness != "" {
		updates["completeness"] = archivers.NormalizeCompletenessState(result.Completeness)
	}
	return db.Model(item).Updates(updates).Error
}

// completeVideoItemFromPrior completes the item as an exact inheritance of the
// earlier one: same media object, same sidecars, same provenance. Used only
// when a metadata refresh failed and the earlier sidecars are all we have.
func completeVideoItemFromPrior(db *gorm.DB, item, prior *models.ArchiveItem) error {
	updates := map[string]interface{}{
		"status":           "completed",
		"storage_key":      prior.StorageKey,
		"extension":        prior.Extension,
		"file_size":        prior.FileSize,
		"metadata_key":     prior.MetadataKey,
		"raw_metadata_key": prior.RawMetadataKey,
	}
	if prior.Source != "" {
		updates["source"] = prior.Source
	}
	if prior.Completeness != "" {
		updates["completeness"] = archivers.NormalizeCompletenessState(prior.Completeness)
	}
	return db.Model(item).Updates(updates).Error
}

// copyPriorThumbnail points the new item at the earlier capture's preview
// object when the refresh produced none of its own. The bucket is append-only,
// so sharing the object is safe; a failure only means the lazy thumbnail
// worker gets a turn later.
func copyPriorThumbnail(db *gorm.DB, item, prior *models.ArchiveItem) {
	if prior.ThumbnailStatus != models.ThumbnailStatusReady || prior.ThumbnailKey == "" {
		return
	}
	if err := db.Model(item).Updates(map[string]interface{}{
		"thumbnail_key":    prior.ThumbnailKey,
		"thumbnail_width":  prior.ThumbnailWidth,
		"thumbnail_height": prior.ThumbnailHeight,
		"thumbnail_status": models.ThumbnailStatusReady,
		"thumbnail_kind":   prior.ThumbnailKind,
	}).Error; err != nil {
		slog.Warn("Failed to copy prior video thumbnail", "item_id", item.ID, "error", err)
	}
}
