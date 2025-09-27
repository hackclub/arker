package workers

import (
	"context"
	"log/slog"

	"gorm.io/gorm"
)

type CleanupWorker struct {
	db *gorm.DB
}

func NewCleanupWorker(db *gorm.DB) *CleanupWorker {
	return &CleanupWorker{db: db}
}

func (w *CleanupWorker) RunCleanup(ctx context.Context) error {
	slog.Info("Starting periodic archive item cleanup")

	// Orphaned pending items older than 1 hour with no corresponding River job
	pendingResult := w.db.Exec(`
		UPDATE archive_items 
		SET status = 'failed', 
			updated_at = NOW(),
			logs = COALESCE(logs, '') || E'\n\n' || 'Marked as failed during periodic cleanup - no corresponding River job found (' || NOW()::timestamptz || ')'
		FROM captures c
		WHERE archive_items.capture_id = c.id
		  AND archive_items.status = 'pending'
		  AND archive_items.updated_at < NOW() - INTERVAL '1 hour'
		  AND NOT EXISTS (
			  SELECT 1 FROM river_job rj 
			  WHERE rj.args::json->>'short_id' = c.short_id 
				AND rj.args::json->>'type' = archive_items.type
				AND rj.state IN ('available', 'running', 'retryable', 'scheduled')
		  )
	`)
	if pendingResult.Error != nil {
		slog.Error("Failed to cleanup pending items", "error", pendingResult.Error)
	} else if pendingResult.RowsAffected > 0 {
		slog.Info("Cleaned up orphaned pending items", "count", pendingResult.RowsAffected)
	}

	// Orphaned processing items older than 1 hour with no running job
	processingResult := w.db.Exec(`
		UPDATE archive_items 
		SET status = 'failed',
			updated_at = NOW(),
			logs = COALESCE(logs, '') || E'\n\n' || 'Marked as failed during periodic cleanup - no running River job found (' || NOW()::timestamptz || ')'
		FROM captures c
		WHERE archive_items.capture_id = c.id
		  AND archive_items.status = 'processing'
		  AND archive_items.updated_at < NOW() - INTERVAL '1 hour'
		  AND NOT EXISTS (
			  SELECT 1 FROM river_job rj 
			  WHERE rj.args::json->>'short_id' = c.short_id 
				AND rj.args::json->>'type' = archive_items.type
				AND rj.state = 'running'
		  )
	`)
	if processingResult.Error != nil {
		slog.Error("Failed to cleanup processing items", "error", processingResult.Error)
	} else if processingResult.RowsAffected > 0 {
		slog.Info("Cleaned up orphaned processing items", "count", processingResult.RowsAffected)
	}

	// Items with discarded jobs
	discardedResult := w.db.Exec(`
		UPDATE archive_items 
		SET status = 'failed',
			updated_at = NOW(),
			logs = COALESCE(logs, '') || E'\n\n' || 'Marked as failed during periodic cleanup - River job was discarded (' || NOW()::timestamptz || ')'
		FROM captures c
		WHERE archive_items.capture_id = c.id
		  AND archive_items.status IN ('pending', 'processing')
		  AND EXISTS (
			  SELECT 1 FROM river_job rj 
			  WHERE rj.args::json->>'short_id' = c.short_id 
				AND rj.args::json->>'type' = archive_items.type
				AND rj.state = 'discarded'
		  )
	`)
	if discardedResult.Error != nil {
		slog.Error("Failed to cleanup items with discarded jobs", "error", discardedResult.Error)
	} else if discardedResult.RowsAffected > 0 {
		slog.Info("Cleaned up items with discarded River jobs", "count", discardedResult.RowsAffected)
	}

	totalCleaned := pendingResult.RowsAffected + processingResult.RowsAffected + discardedResult.RowsAffected
	slog.Info("Periodic cleanup completed", "total_cleaned", totalCleaned)

	return nil
}
