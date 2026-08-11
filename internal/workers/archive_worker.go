package workers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

// ArchiveJobArgs represents the payload for an archive job in River.
type ArchiveJobArgs struct {
	CaptureID uint   `json:"capture_id"`
	ShortID   string `json:"short_id"`
	Type      string `json:"type"`
	URL       string `json:"url"`
}

// Kind returns the job kind for River.
func (ArchiveJobArgs) Kind() string { return "archive" }

// ArchiveWorker processes archive jobs using River.
type ArchiveWorker struct {
	river.WorkerDefaults[ArchiveJobArgs]
	storage      storage.Storage
	db           *gorm.DB
	archiversMap map[string]archivers.Archiver
}

// NewArchiveWorker creates a new archive worker.
func NewArchiveWorker(storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) *ArchiveWorker {
	return &ArchiveWorker{
		storage:      storage,
		db:           db,
		archiversMap: archiversMap,
	}
}

// Work processes a single archive job from the queue.
func (w *ArchiveWorker) Work(ctx context.Context, job *river.Job[ArchiveJobArgs]) error {
	args := job.Args
	// Jobs enqueued before the yt-dlp rename carry the old type in their args
	// and must still find their archive item and archiver.
	args.Type = utils.NormalizeArchiveType(args.Type)
	logger := slog.With(
		"worker", "river",
		"job_id", job.ID,
		"attempt", job.Attempt,
		"short_id", args.ShortID,
		"type", args.Type,
	)

	logger.Info("Processing archive job")

	var item models.ArchiveItem
	if err := w.db.Joins("JOIN captures ON archive_items.capture_id = captures.id").
		Where("captures.short_id = ? AND archive_items.type = ?", args.ShortID, args.Type).
		First(&item).Error; err != nil {
		logger.Error("Failed to find archive item", "error", err)
		return fmt.Errorf("archive item not found for short_id %s and type %s: %w", args.ShortID, args.Type, err)
	}

	// Update status to processing and set retry count.
	w.db.Model(&item).Updates(map[string]interface{}{
		"status":      "processing",
		"retry_count": job.Attempt,
	})
	item.RetryCount = job.Attempt

	// Process the job. This function contains its own timeout logic.
	err := processArchiveJob(ctx, args, &item, w.storage, w.db, w.archiversMap)

	if err != nil {
		logger.Error("Job processing failed", "error", err)

		// On the final attempt, mark as failed permanently and append a clear message
		if job.Attempt >= job.MaxAttempts {
			_ = w.db.Model(&item).Updates(map[string]interface{}{
				"status":     "failed",
				"updated_at": time.Now(),
			}).Error
			_ = utils.AppendArchiveItemLog(w.db, item.ID, job.Attempt, fmt.Sprintf("\n\nFinal attempt failed after %d tries: %v", job.MaxAttempts, err))
			slog.Error("Archive job permanently failed",
				"short_id", args.ShortID, "type", args.Type,
				"attempts", job.MaxAttempts, "error", err)
		}
		// Let River retry (if any attempts left)
		return err
	}

	logger.Info("Job processing completed successfully")
	return nil
}

// processArchiveJob handles the logic for a single job attempt.
func processArchiveJob(ctx context.Context, jobArgs ArchiveJobArgs, item *models.ArchiveItem, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	arch, ok := archiversMap[jobArgs.Type]
	if !ok {
		return fmt.Errorf("unknown archiver %s", jobArgs.Type)
	}

	dbLogWriter := utils.NewDBLogWriterForAttempt(db, item.ID, item.RetryCount)
	defer func() {
		if err := dbLogWriter.Flush(); err != nil {
			slog.Error("Failed to flush archive logs", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		}
	}()

	slog.Info("Starting archive operation",
		"short_id", jobArgs.ShortID,
		"type", jobArgs.Type,
		"url", jobArgs.URL,
		"attempt", item.RetryCount)

	timeout := utils.TimeoutForArchiveJob(ctx, jobArgs.Type, jobArgs.URL, dbLogWriter)
	ctx, cancel := context.WithTimeout(ctx, timeout) // respect River cancellation
	defer cancel()

	// Archive the content. PWBundle is returned for browser-based archivers.
	result, err := arch.Archive(ctx, jobArgs.URL, dbLogWriter, db, item.ID)

	// CRITICAL: Always defer bundle cleanup to ensure the browser is closed.
	if result.Bundle != nil {
		defer result.Bundle.Cleanup()
	}

	if err != nil {
		slog.Error("Archive operation failed", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		return err
	}

	// Save the resulting data to storage. The archive bucket forbids
	// overwrites and deletes (bucket lock), so every upload attempt writes a
	// fresh object; the item's storage_key records the key that succeeded.
	nonce := uploadNonce()
	keyBase := fmt.Sprintf("%s/%s-%s", jobArgs.ShortID, jobArgs.Type, nonce)
	key := keyBase + result.Extension
	err = saveArchiveResult(result, keyBase, storage, db, item)
	if err != nil {
		slog.Error("Failed to save archive data", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
		fmt.Fprintf(dbLogWriter, "\nFailed to save archive data: %v\n", err)
		return err
	}

	slog.Info("Archive saved successfully",
		"short_id", jobArgs.ShortID,
		"type", jobArgs.Type,
		"storage_key", key)

	// Persist the thumbnail after the archive is already marked completed, and
	// never propagate its error. The archive is the product; the preview is
	// not, and failing an otherwise-good capture over a cosmetic artifact would
	// burn a River attempt and re-run the whole download.
	if result.Thumbnail != nil {
		thumbKey := fmt.Sprintf("%s/%s-%s-thumb%s", jobArgs.ShortID, jobArgs.Type, nonce, thumbnail.Extension)
		if err := StoreThumbnail(result.Thumbnail, thumbKey, storage, db, item); err != nil {
			slog.Warn("Failed to save thumbnail", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
			fmt.Fprintf(dbLogWriter, "\nWarning: failed to save thumbnail: %v\n", err)
		}
	}

	return nil
}

// saveArchiveResult stores the primary artifact and every sidecar before it
// marks the database item completed. The bucket is append-only, so a failure
// can leave unreachable objects behind, but it can never publish a completed
// item whose required metadata is only partly stored.
func saveArchiveResult(result archivers.Result, keyBase string, store storage.Storage, db *gorm.DB, item *models.ArchiveItem) error {
	key := keyBase + result.Extension
	fileSize, err := writeArchiveData(result.Data, key, store)
	if err != nil {
		return err
	}

	metadataKey := ""
	if result.Metadata != nil {
		metadataKey = keyBase + ".metadata.json"
		if err := writeJSONSidecar(store, metadataKey, result.Metadata.Data); err != nil {
			return fmt.Errorf("failed to store normalized metadata: %w", err)
		}
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
		"storage_key":      key,
		"extension":        result.Extension,
		"file_size":        fileSize,
		"metadata_key":     metadataKey,
		"raw_metadata_key": rawMetadataKey,
	}
	if result.Source != "" {
		updates["source"] = result.Source
	}
	return db.Model(item).Updates(updates).Error
}

func writeJSONSidecar(store storage.Storage, key string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("sidecar is empty")
	}
	if !json.Valid(data) {
		return fmt.Errorf("sidecar is not valid JSON")
	}
	w, err := store.Writer(key)
	if err != nil {
		return err
	}
	_, writeErr := io.Copy(w, bytes.NewReader(data))
	if closeErr := w.Close(); closeErr != nil && writeErr == nil {
		writeErr = closeErr
	}
	return writeErr
}

// StoreThumbnail writes an encoded thumbnail to storage and points the archive
// item at it.
//
// Like archive artifacts, thumbnail keys carry an upload nonce: the bucket
// forbids overwrites and deletes, so regenerating a thumbnail means writing a
// new object and repointing the row, never replacing bytes in place.
func StoreThumbnail(thumb *archivers.Thumbnail, key string, store storage.Storage, db *gorm.DB, item *models.ArchiveItem) error {
	if thumb == nil || len(thumb.Data) == 0 {
		return fmt.Errorf("thumbnail is empty")
	}

	w, err := store.Writer(key)
	if err != nil {
		return fmt.Errorf("failed to get thumbnail writer: %w", err)
	}
	_, copyErr := w.Write(thumb.Data)
	if closeErr := w.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("failed writing thumbnail: %w", copyErr)
	}

	return db.Model(item).Updates(map[string]interface{}{
		"thumbnail_key":    key,
		"thumbnail_width":  thumb.Width,
		"thumbnail_height": thumb.Height,
		"thumbnail_status": models.ThumbnailStatusReady,
	}).Error
}

// uploadNonce returns a short random suffix for storage keys so retries never
// overwrite an existing (locked) object.
func uploadNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// saveArchiveData handles writing archive data to storage and updating the database.
func saveArchiveData(data io.Reader, key, ext, source string, storage storage.Storage, db *gorm.DB, item *models.ArchiveItem) error {
	fileSize, err := writeArchiveData(data, key, storage)
	if err != nil {
		return err
	}

	// Mark as completed and store final metadata.
	updates := map[string]interface{}{
		"status":      "completed",
		"storage_key": key,
		"extension":   ext,
		"file_size":   fileSize,
	}
	// Source is only written when the archiver declared one (the Bright Data
	// fallback does); native archivers leave the column at its default.
	if source != "" {
		updates["source"] = source
	}
	return db.Model(item).Updates(updates).Error
}

func writeArchiveData(data io.Reader, key string, storage storage.Storage) (int64, error) {
	if data == nil {
		return 0, fmt.Errorf("archive data is nil")
	}
	// Archivers hand back readers backed by a live process or a goroutine
	// writing into an io.Pipe, and closing is what releases them. Every return
	// path below must close, including the early one when Writer fails: an
	// unclosed pipe leaves that goroutine blocked on Write forever, holding its
	// temp directory (a whole gallery-dl download) for the life of the process.
	closed := false
	closeData := func() error {
		if closed {
			return nil
		}
		closed = true
		if c, ok := data.(io.Closer); ok {
			return c.Close()
		}
		return nil
	}
	defer closeData()

	w, err := storage.Writer(key)
	if err != nil {
		return 0, fmt.Errorf("failed to get storage writer: %w", err)
	}

	_, copyErr := io.Copy(w, data)

	// For archivers that return a process (like yt-dlp), we must close the reader to wait for the process to exit.
	if closeErr := closeData(); closeErr != nil && copyErr == nil {
		copyErr = closeErr // Prioritize the close error if copy was successful.
	}

	if closeErr := w.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr // Prioritize writer close error.
	}

	if copyErr != nil {
		return 0, fmt.Errorf("failed during data copy/close: %w", copyErr)
	}

	fileSize, err := storage.Size(key)
	if err != nil {
		log.Printf("Warning: Could not get file size for %s: %v", key, err)
		fileSize = 0
	}
	return fileSize, nil
}
