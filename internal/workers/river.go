package workers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// ArchiveJobArgs represents the payload for an archive job in River
type ArchiveJobArgs struct {
	CaptureID uint   `json:"capture_id"`
	ShortID   string `json:"short_id"`
	Type      string `json:"type"`
	URL       string `json:"url"`
}

// Kind returns the job kind for River
func (ArchiveJobArgs) Kind() string { return "archive" }

// ArchiveWorker processes archive jobs using River
type ArchiveWorker struct {
	river.WorkerDefaults[ArchiveJobArgs]
	storage      storage.Storage
	db           *gorm.DB
	archiversMap map[string]archivers.Archiver
}

// NewArchiveWorker creates a new archive worker
func NewArchiveWorker(storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) *ArchiveWorker {
	return &ArchiveWorker{
		storage:      storage,
		db:           db,
		archiversMap: archiversMap,
	}
}

// Work processes an archive job
func (w *ArchiveWorker) Work(ctx context.Context, job *river.Job[ArchiveJobArgs]) error {
	args := job.Args
	logger := slog.With(
		"worker", "river",
		"job_id", job.ID,
		"attempt", job.Attempt,
		"short_id", args.ShortID,
		"type", args.Type,
		"url", args.URL,
	)

	logger.Info("Processing archive job")

	// Find the archive item
	var item models.ArchiveItem
	if args.CaptureID != 0 {
		// New format with CaptureID
		if err := w.db.Where("capture_id = ? AND type = ?", args.CaptureID, args.Type).First(&item).Error; err != nil {
			logger.Error("Failed to find archive item by capture_id", "error", err)
			return fmt.Errorf("archive item not found: %w", err)
		}
	} else {
		// Legacy format - lookup by short_id and type
		if err := w.db.Joins("JOIN captures ON archive_items.capture_id = captures.id").
			Where("captures.short_id = ? AND archive_items.type = ?", args.ShortID, args.Type).
			First(&item).Error; err != nil {
			logger.Error("Failed to find archive item by short_id", "error", err)
			return fmt.Errorf("archive item not found: %w", err)
		}
		// Update the job args with the correct CaptureID for compatibility
		args.CaptureID = item.CaptureID
	}

	// Update status to processing and set retry count
	w.db.Model(&item).Updates(map[string]interface{}{
		"status":      "processing",
		"retry_count": job.Attempt,
	})

	// Convert River job args to our Job model for compatibility
	arkerJob := models.Job{
		CaptureID: args.CaptureID,
		ShortID:   args.ShortID,
		Type:      args.Type,
		URL:       args.URL,
	}

	// Process the job using existing logic (ProcessSingleJob has its own timeout handling)
	err := ProcessSingleJob(arkerJob, w.storage, w.db, w.archiversMap)
	
	if err != nil {
		logger.Error("Job processing failed", "error", err)
		
		// River will handle retries automatically, we just mark failed on final attempt
		if job.Attempt+1 >= job.MaxAttempts {
			w.db.Model(&item).Update("status", "failed")
		}
		
		return err
	}

	logger.Info("Job processing completed successfully")
	return nil
}

// archiveTimeoutForType returns appropriate timeout for different job types
func archiveTimeoutForType(jobType string) time.Duration {
	timeoutConfig := utils.DefaultTimeoutConfig()
	switch jobType {
	case "git":
		return timeoutConfig.GitCloneTimeout
	case "youtube":
		return timeoutConfig.YtDlpTimeout
	default:
		return timeoutConfig.ArchiveTimeout
	}
}

// BulkRetryJobArgs represents the payload for a bulk retry job in River
type BulkRetryJobArgs struct {
	RequestedBy string `json:"requested_by"` // Admin user who requested the retry
}

// Kind returns the job kind for River
func (BulkRetryJobArgs) Kind() string { return "bulk_retry" }

// BulkRetryWorker processes bulk retry jobs using River
type BulkRetryWorker struct {
	river.WorkerDefaults[BulkRetryJobArgs]
	db                *gorm.DB
	riverQueueManager *RiverQueueManager
}

// NewBulkRetryWorker creates a new bulk retry worker
func NewBulkRetryWorker(db *gorm.DB, riverQueueManager *RiverQueueManager) *BulkRetryWorker {
	return &BulkRetryWorker{
		db:                db,
		riverQueueManager: riverQueueManager,
	}
}

// Work processes a bulk retry job
func (w *BulkRetryWorker) Work(ctx context.Context, job *river.Job[BulkRetryJobArgs]) error {
	logger := slog.With(
		"worker", "bulk_retry",
		"job_id", job.ID,
		"requested_by", job.Args.RequestedBy,
	)

	logger.Info("Starting bulk retry of failed jobs")

	// Get all failed items with their capture information in batches
	var totalRetried int
	batchSize := 100

	for {
		var failedItems []models.ArchiveItem
		if err := w.db.Where("status = 'failed'").Limit(batchSize).Find(&failedItems).Error; err != nil {
			logger.Error("Failed to find failed items", "error", err)
			return err
		}

		if len(failedItems) == 0 {
			break // No more failed items
		}

		retriedInBatch := 0

		for _, item := range failedItems {
			// Get the capture and URL information
			var capture models.Capture
			if err := w.db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
				logger.Warn("Skipping item with missing capture", "item_id", item.ID, "capture_id", item.CaptureID)
				continue
			}

			// Reset the item status to pending and reset retry count
			currentTime := time.Now()
			item.Status = "pending"
			item.RetryCount = 0
			item.Logs = "Bulk retry at " + currentTime.Format("2006-01-02 15:04:05") + " (retry count reset)\n" + item.Logs

			if err := w.db.Save(&item).Error; err != nil {
				logger.Warn("Failed to save item", "item_id", item.ID, "error", err)
				continue
			}

			// Enqueue the job in River
			args := ArchiveJobArgs{
				CaptureID: capture.ID,
				ShortID:   capture.ShortID,
				Type:      item.Type,
				URL:       capture.ArchivedURL.Original,
			}

			opts := &river.InsertOpts{
				MaxAttempts: 3,
				Queue:       "high_priority", // Use high-priority queue for retry jobs
				Tags:        []string{"archive", item.Type, "bulk_retry"},
			}

			if _, err := w.riverQueueManager.RiverClient.Insert(ctx, args, opts); err != nil {
				// If enqueueing fails, mark item as failed again
				item.Status = "failed"
				item.Logs = item.Logs + "\nFailed to enqueue retry in River: " + err.Error()
				w.db.Save(&item)
				logger.Warn("Failed to enqueue retry", "item_id", item.ID, "error", err)
				continue
			}

			retriedInBatch++
		}

		totalRetried += retriedInBatch
		logger.Info("Processed batch", "batch_size", len(failedItems), "retried", retriedInBatch, "total_retried", totalRetried)

		// Small delay to avoid overwhelming the system
		time.Sleep(10 * time.Millisecond)
	}

	logger.Info("Bulk retry completed", "total_retried", totalRetried)
	return nil
}
