package workers

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"
	"gorm.io/gorm"
	"github.com/klauspost/compress/zstd"
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
	for job := range jobChan {
		err := ProcessJob(job, storage, db, archiversMap)
		if err != nil {
			log.Printf("Worker %d failed job %s %s: %v", id, job.ShortID, job.Type, err)
			db.Model(&models.ArchiveItem{}).Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).Update("status", "failed")
		} else {
			log.Printf("Worker %d completed %s %s", id, job.ShortID, job.Type)
		}
	}
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
		return ProcessSingleJob(job, storage, db, archiversMap)
	}
	
	log.Printf("Processing combined browser job for %s: %v", job.ShortID, pendingBrowserTypes)
	
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
	
	// Create shared page with screenshot settings (need viewport for screenshots)
	page, err := mhtmlArch.Browser.NewPage(playwright.BrowserNewPageOptions{
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
	defer page.Close()
	
	// Create shared log writer for page load
	sharedLogWriter := utils.NewDBLogWriter(db, 0) // Use 0 since we'll update both items separately
	
	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(sharedLogWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(sharedLogWriter, "Page error: %v\n", err)
	})
	
	log.Printf("Starting shared page load for combined job: %s", job.ShortID)
	fmt.Fprintf(sharedLogWriter, "Starting combined browser job for MHTML and screenshot\n")
	
	// Single page load for both
	if err := archivers.PerformCompletePageLoad(page, job.URL, sharedLogWriter, true); err != nil {
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
	
	mhtmlData, mhtmlExt, _, _, err := mhtmlArch.ArchiveWithPage(page, job.URL, mhtmlLogWriter)
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
	
	screenshotData, screenshotExt, _, _, err := screenshotArch.ArchiveWithPage(page, job.URL, screenshotLogWriter, nil)
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
	
	log.Printf("Completed combined browser job for %s", job.ShortID)
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
	log.Printf("Starting archive job: %s %s", job.ShortID, job.Type)
	
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
	
	err := utils.WithRetryConfig(func() error {
		// Wrap archive operation with timeout
		return utils.WithTimeout(timeout, func(ctx context.Context) error {
			var archiveErr error
			data, ext, _, cleanup, archiveErr = arch.Archive(job.URL, dbLogWriter, db, item.ID)
			// TODO: Update archiver interface to support context cancellation
			return archiveErr
		})
	}, dbLogWriter, &currentRetryCount, retryConfig)
	
	log.Printf("Archive job returned: %s %s, error: %v", job.ShortID, job.Type, err)
	if err != nil {
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
	defer w.Close()
	
	zw, err := zstd.NewWriter(w)
	if err != nil {
		db.Model(item).Updates(map[string]interface{}{
			"status": "failed", 
			"logs":   logWriter.String(),
		})
		return err
	}
	defer zw.Close()
	
	if _, err = io.Copy(zw, data); err != nil {
		db.Model(item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   logWriter.String(),
		})
		return err
	}
	
	// Mark as completed and store final logs
	db.Model(item).Updates(map[string]interface{}{
		"status":      "completed",
		"storage_key": key,
		"extension":   ext,
		"logs":        logWriter.String(),
	})
	return nil
}
