package workers

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"time"
	"gorm.io/gorm"
	"github.com/playwright-community/playwright-go"
	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// Queue channel
var JobChan = make(chan models.Job, 100)

// Worker
func Worker(id int, jobChan <-chan models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) {
	logger := slog.With("worker_id", id)
	logger.Info("Worker started")
	
	for job := range jobChan {
		jobStart := time.Now()
		
		logger.Info("Processing job",
			"short_id", job.ShortID,
			"type", job.Type,
			"url", job.URL,
			"capture_id", job.CaptureID)
		
		err := ProcessJob(job, storage, db, archiversMap)
		duration := time.Since(jobStart)
		
		if err != nil {
			logger.Error("Job processing failed",
				"short_id", job.ShortID,
				"type", job.Type,
				"url", job.URL,
				"duration", duration.Round(time.Millisecond),
				"error", err)
			db.Model(&models.ArchiveItem{}).Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).Update("status", "failed")
		} else {
			logger.Info("Job processing completed",
				"short_id", job.ShortID,
				"type", job.Type,
				"url", job.URL,
				"duration", duration.Round(time.Millisecond))
		}
	}
	
	logger.Info("Worker stopped")
}

// Process job (streams to zstd/FS)
func ProcessJob(job models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	// Check if this job could benefit from page reuse (browser-based jobs)
	if job.Type == "mhtml" || job.Type == "screenshot" {
		return ProcessCombinedBrowserJob(job, storage, db, archiversMap)
	}
	
	return ProcessSingleJob(job, storage, db, archiversMap)
}

// ProcessCombinedBrowserJob handles MHTML and screenshot on the same page if both are needed
func ProcessCombinedBrowserJob(job models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	// Check if both mhtml and screenshot are pending for this capture
	var pendingBrowserTypes []string
	db.Model(&models.ArchiveItem{}).Where("capture_id = ? AND type IN (?) AND status = ?", 
		job.CaptureID, []string{"mhtml", "screenshot"}, "pending").Pluck("type", &pendingBrowserTypes)
	
	if len(pendingBrowserTypes) < 2 {
		// Only one type is pending, fallback to single processing
		slog.Debug("Combined job optimization not available, using single processing",
			"short_id", job.ShortID,
			"pending_types", pendingBrowserTypes)
		return ProcessSingleJob(job, storage, db, archiversMap)
	}
	
	slog.Info("Starting combined browser job optimization",
		"short_id", job.ShortID,
		"types", pendingBrowserTypes,
		"url", job.URL)
	
	// Get both items and update their status
	var mhtmlItem, screenshotItem models.ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", job.CaptureID, "mhtml").First(&mhtmlItem).Error; err != nil {
		return fmt.Errorf("failed to get mhtml item: %v", err)
	}
	if err := db.Where("capture_id = ? AND type = ?", job.CaptureID, "screenshot").First(&screenshotItem).Error; err != nil {
		return fmt.Errorf("failed to get screenshot item: %v", err)
	}
	
	// Check retry limits
	if mhtmlItem.RetryCount >= 3 || screenshotItem.RetryCount >= 3 {
		// Mark failed items and process the other one normally
		if mhtmlItem.RetryCount >= 3 {
			db.Model(&mhtmlItem).Update("status", "failed")
		}
		if screenshotItem.RetryCount >= 3 {
			db.Model(&screenshotItem).Update("status", "failed")
		}
		// Process whichever one can still be retried
		if mhtmlItem.RetryCount < 3 && job.Type == "mhtml" {
			return ProcessSingleJob(job, storage, db, archiversMap)
		}
		if screenshotItem.RetryCount < 3 && job.Type == "screenshot" {
			return ProcessSingleJob(job, storage, db, archiversMap)
		}
		return fmt.Errorf("max retries exceeded for combined job %s", job.ShortID)
	}
	
	// Update both to processing and increment retry counts
	db.Model(&mhtmlItem).Updates(map[string]interface{}{
		"status":      "processing",
		"retry_count": gorm.Expr("retry_count + 1"),
	})
	db.Model(&screenshotItem).Updates(map[string]interface{}{
		"status":      "processing", 
		"retry_count": gorm.Expr("retry_count + 1"),
	})
	
	// Get archivers
	mhtmlArch := archiversMap["mhtml"].(*archivers.MHTMLArchiver)
	screenshotArch := archiversMap["screenshot"].(*archivers.ScreenshotArchiver)
	
	// Create context with timeout for combined job
	timeoutConfig := utils.DefaultTimeoutConfig()
	ctx, cancel := context.WithTimeout(context.Background(), timeoutConfig.ArchiveTimeout)
	defer cancel()
	
	// Create shared page with screenshot settings (need viewport for screenshots)
	page, err := mhtmlArch.BrowserMgr.NewPage(playwright.BrowserNewPageOptions{
		Viewport: &playwright.Size{
			Width:  1500,
			Height: 1080,
		},
		DeviceScaleFactor: playwright.Float(2.0), // Retina quality
	})
	if err != nil {
		// Mark both as failed
		db.Model(&mhtmlItem).Update("status", "failed")
		db.Model(&screenshotItem).Update("status", "failed")
		return fmt.Errorf("failed to create shared browser page: %v", err)
	}
	defer mhtmlArch.BrowserMgr.ClosePage(page)
	
	// Create shared log writer for page load
	sharedLogWriter := utils.NewDBLogWriter(db, 0) // Use 0 since we'll update both items separately
	
	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(sharedLogWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(sharedLogWriter, "Page error: %v\n", err)
	})
	
	slog.Info("Starting shared page load for combined job",
		"short_id", job.ShortID,
		"url", job.URL)
	fmt.Fprintf(sharedLogWriter, "Starting combined browser job for MHTML and screenshot\n")
	
	// Single page load for both
	if err := archivers.PerformCompletePageLoadWithContext(ctx, page, job.URL, sharedLogWriter, true); err != nil {
		fmt.Fprintf(sharedLogWriter, "Shared page load failed: %v\n", err)
		// Mark both as failed with shared logs
		sharedLogs := sharedLogWriter.String()
		db.Model(&mhtmlItem).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   sharedLogs,
		})
		db.Model(&screenshotItem).Updates(map[string]interface{}{
			"status": "failed", 
			"logs":   sharedLogs,
		})
		return fmt.Errorf("shared page load failed: %v", err)
	}
	
	// Process MHTML first (uses CDP session)
	mhtmlLogWriter := utils.NewDBLogWriter(db, mhtmlItem.ID)
	fmt.Fprintf(mhtmlLogWriter, "%s\n--- Starting MHTML capture on shared page ---\n", sharedLogWriter.String())
	
	mhtmlData, mhtmlExt, _, _, err := mhtmlArch.ArchiveWithPageContext(ctx, page, job.URL, mhtmlLogWriter)
	if err != nil {
		fmt.Fprintf(mhtmlLogWriter, "MHTML capture failed: %v\n", err)
		db.Model(&mhtmlItem).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   mhtmlLogWriter.String(),
		})
	} else {
		// Save MHTML data
		if err := saveArchiveData(mhtmlData, mhtmlExt, job.ShortID, "mhtml", storage, db, &mhtmlItem, mhtmlLogWriter); err != nil {
			log.Printf("Failed to save MHTML data: %v", err)
		}
	}
	
	// Then screenshot on the same page
	screenshotLogWriter := utils.NewDBLogWriter(db, screenshotItem.ID)
	fmt.Fprintf(screenshotLogWriter, "%s\n--- Starting screenshot capture on shared page ---\n", sharedLogWriter.String())
	
	screenshotData, screenshotExt, _, _, err := screenshotArch.ArchiveWithPageContext(ctx, page, job.URL, screenshotLogWriter, nil)
	if err != nil {
		fmt.Fprintf(screenshotLogWriter, "Screenshot capture failed: %v\n", err)
		db.Model(&screenshotItem).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   screenshotLogWriter.String(),
		})
	} else {
		// Save screenshot data  
		if err := saveArchiveData(screenshotData, screenshotExt, job.ShortID, "screenshot", storage, db, &screenshotItem, screenshotLogWriter); err != nil {
			log.Printf("Failed to save screenshot data: %v", err)
		}
	}
	
	slog.Info("Completed combined browser job",
		"short_id", job.ShortID,
		"types", pendingBrowserTypes)
	return nil
}

// ProcessSingleJob handles individual job processing (original logic)
func ProcessSingleJob(job models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	var item models.ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).First(&item).Error; err != nil {
		return err
	}
	
	// Check retry limit
	if item.RetryCount >= 3 {
		db.Model(&item).Update("status", "failed")
		return fmt.Errorf("max retries exceeded for %s %s", job.ShortID, job.Type)
	}
	
	// Update status to processing and increment retry count
	db.Model(&item).Updates(map[string]interface{}{
		"status":      "processing",
		"retry_count": gorm.Expr("retry_count + 1"),
	})
	
	arch, ok := archiversMap[job.Type]
	if !ok {
		return fmt.Errorf("unknown archiver %s", job.Type)
	}
	
	dbLogWriter := utils.NewDBLogWriter(db, item.ID)
	
	slog.Info("Starting single job processing",
		"short_id", job.ShortID,
		"type", job.Type,
		"url", job.URL,
		"retry_count", item.RetryCount)
	
	// Use error handling wrapper with timeout for the archiving operation
	var data io.Reader
	var ext string
	var cleanup func()
	
	retryConfig := utils.DefaultRetryConfig()
	retryConfig.MaxRetries = 2 // Allow 2 retries beyond the initial attempt (total 3 attempts)
	currentRetryCount := 0
	
	// Get appropriate timeout for the job type
	timeoutConfig := utils.DefaultTimeoutConfig()
	var timeout time.Duration
	switch job.Type {
	case "git":
		timeout = timeoutConfig.GitCloneTimeout
	case "youtube":
		timeout = timeoutConfig.YtDlpTimeout
	default:
		timeout = timeoutConfig.ArchiveTimeout
	}
	
	// Create context with timeout for the entire operation
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	
	err := utils.WithRetryConfigContext(ctx, func() error {
		var archiveErr error
		data, ext, _, cleanup, archiveErr = arch.Archive(ctx, job.URL, dbLogWriter, db, item.ID)
		return archiveErr
	}, dbLogWriter, &currentRetryCount, retryConfig)
	
	if err != nil {
		slog.Error("Archive operation failed",
			"short_id", job.ShortID,
			"type", job.Type,
			"url", job.URL,
			"retry_count", item.RetryCount,
			"error", err)
		if cleanup != nil {
			cleanup()
		}
		// Store final logs and mark as failed
		db.Model(&item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   dbLogWriter.String(),
		})
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	
	return saveArchiveData(data, ext, job.ShortID, job.Type, storage, db, &item, dbLogWriter)
}



// saveArchiveData handles the common logic for saving archive data to storage
func saveArchiveData(data io.Reader, ext, shortID, jobType string, storage storage.Storage, db *gorm.DB, item *models.ArchiveItem, logWriter *utils.DBLogWriter) error {

	
	key := fmt.Sprintf("%s/%s%s.zst", shortID, jobType, ext)
	w, err := storage.Writer(key)
	if err != nil {
		db.Model(item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   logWriter.String(),
		})
		return err
	}
	
	_, err = io.Copy(w, data)
	if err != nil {
		w.Close() // Close on error
		db.Model(item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   logWriter.String(),
		})
		return err
	}
	
	// Close writer to ensure all data is written and compressed
	if err := w.Close(); err != nil {
		db.Model(item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   logWriter.String(),
		})
		return fmt.Errorf("failed to close writer: %v", err)
	}
	
	// Get file size after writing and closing
	fileSize, err := storage.Size(key)
	if err != nil {
		log.Printf("Warning: Could not get file size for %s: %v", key, err)
		fileSize = 0 // Continue without file size if we can't get it
	}
	
	// Mark as completed and store final logs with file size
	db.Model(item).Updates(map[string]interface{}{
		"status":      "completed",
		"storage_key": key,
		"extension":   ext,
		"file_size":   fileSize,
		"logs":        logWriter.String(),
	})
	
	slog.Info("Archive saved successfully",
		"short_id", shortID,
		"type", jobType,
		"file_size", fileSize,
		"storage_key", key)
	
	return nil
}
