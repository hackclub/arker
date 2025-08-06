package workers

import (
    "context"
    "fmt"
    "io"
    "log"
    "log/slog"

    "github.com/riverqueue/river"
    "gorm.io/gorm"

    "arker/internal/archivers"
    "arker/internal/models"
    "arker/internal/storage"
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

    // Process the job. This function contains its own timeout logic.
    err := processArchiveJob(args, &item, w.storage, w.db, w.archiversMap)

    if err != nil {
        logger.Error("Job processing failed", "error", err)
        // On the final attempt, mark as failed permanently.
        if job.Attempt+1 >= job.MaxAttempts {
            w.db.Model(&item).Update("status", "failed")
        }
        // Return the error to let River handle the retry.
        return err
    }

    logger.Info("Job processing completed successfully")
    return nil
}

// processArchiveJob handles the logic for a single job attempt.
func processArchiveJob(jobArgs ArchiveJobArgs, item *models.ArchiveItem, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
    arch, ok := archiversMap[jobArgs.Type]
    if !ok {
        return fmt.Errorf("unknown archiver %s", jobArgs.Type)
    }

    dbLogWriter := utils.NewDBLogWriter(db, item.ID)

    slog.Info("Starting archive operation",
        "short_id", jobArgs.ShortID,
        "type", jobArgs.Type,
        "url", jobArgs.URL,
        "attempt", item.RetryCount)

    timeout := utils.TimeoutForJobType(jobArgs.Type)
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // Archive the content. PWBundle is returned for browser-based archivers.
    data, ext, _, bundle, err := arch.Archive(ctx, jobArgs.URL, dbLogWriter, db, item.ID)

    // CRITICAL: Always defer bundle cleanup to ensure the browser is closed.
    if bundle != nil {
        defer bundle.Cleanup()
    }

    if err != nil {
        slog.Error("Archive operation failed", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
        db.Model(item).Updates(map[string]interface{}{"status": "failed", "logs": dbLogWriter.String()})
        return err
    }

    // Save the resulting data to storage.
    key := fmt.Sprintf("%s/%s%s.zst", jobArgs.ShortID, jobArgs.Type, ext)
    err = saveArchiveData(data, key, ext, storage, db, item, dbLogWriter)
    if err != nil {
        slog.Error("Failed to save archive data", "short_id", jobArgs.ShortID, "type", jobArgs.Type, "error", err)
        db.Model(item).Updates(map[string]interface{}{"status": "failed", "logs": dbLogWriter.String()})
        return err
    }

    slog.Info("Archive saved successfully",
        "short_id", jobArgs.ShortID,
        "type", jobArgs.Type,
        "storage_key", key)

    return nil
}

// saveArchiveData handles writing archive data to storage and updating the database.
func saveArchiveData(data io.Reader, key, ext string, storage storage.Storage, db *gorm.DB, item *models.ArchiveItem, logWriter *utils.DBLogWriter) error {
    w, err := storage.Writer(key)
    if err != nil {
        return fmt.Errorf("failed to get storage writer: %w", err)
    }

    _, copyErr := io.Copy(w, data)

    // For archivers that return a process (like yt-dlp), we must close the reader to wait for the process to exit.
    if c, ok := data.(io.Closer); ok {
        if closeErr := c.Close(); closeErr != nil && copyErr == nil {
            copyErr = closeErr // Prioritize the close error if copy was successful.
        }
    }

    if closeErr := w.Close(); closeErr != nil && copyErr == nil {
        copyErr = closeErr // Prioritize writer close error.
    }
    
    if copyErr != nil {
        return fmt.Errorf("failed during data copy/close: %w", copyErr)
    }

    fileSize, err := storage.Size(key)
    if err != nil {
        log.Printf("Warning: Could not get file size for %s: %v", key, err)
        fileSize = 0
    }

    // Mark as completed and store final metadata.
    return db.Model(item).Updates(map[string]interface{}{
        "status":      "completed",
        "storage_key": key,
        "extension":   ext,
        "file_size":   fileSize,
        "logs":        logWriter.String(),
    }).Error
}
