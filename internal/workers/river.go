package workers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
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






