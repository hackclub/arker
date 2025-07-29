package workers

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"gorm.io/gorm"
	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// prefixWriter wraps stdout with a prefix for each line
type prefixWriter struct {
	prefix string
}

func (pw *prefixWriter) Write(p []byte) (n int, err error) {
	// Convert to string and add prefix to each line
	s := string(p)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			// Don't add prefix to trailing empty line
			continue
		}
		prefixedLine := pw.prefix + line
		if i < len(lines)-1 {
			prefixedLine += "\n"
		}
		fmt.Fprint(os.Stdout, prefixedLine)
		if i == len(lines)-1 && line != "" {
			// Add newline if the last line doesn't end with one
			fmt.Fprint(os.Stdout, "\n")
		}
	}
	return len(p), nil
}

// Queue channel
var JobChan = make(chan models.Job, 100)

// Worker heartbeat tracking
var workerHeartbeats = make(map[int]time.Time)
var heartbeatMutex sync.RWMutex

// Worker
func Worker(id int, jobChan <-chan models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) {
	logger := slog.With("worker_id", id)
	logger.Info("Worker started")
	
	// Initialize heartbeat
	heartbeatMutex.Lock()
	workerHeartbeats[id] = time.Now()
	heartbeatMutex.Unlock()
	
	// Start heartbeat ticker for idle workers
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()
	
	go func() {
		for range heartbeatTicker.C {
			heartbeatMutex.Lock()
			workerHeartbeats[id] = time.Now()
			heartbeatMutex.Unlock()
		}
	}()
	
	for job := range jobChan {
		jobStart := time.Now()
		
		// Update worker heartbeat
		heartbeatMutex.Lock()
		workerHeartbeats[id] = time.Now()
		heartbeatMutex.Unlock()
		
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
		
		// Update heartbeat after job completion
		heartbeatMutex.Lock()
		workerHeartbeats[id] = time.Now()
		heartbeatMutex.Unlock()
	}
	
	logger.Info("Worker stopped")
}

// GetWorkerStatus returns the status of all workers
func GetWorkerStatus() map[string]interface{} {
	heartbeatMutex.RLock()
	defer heartbeatMutex.RUnlock()
	
	now := time.Now()
	activeWorkers := 0
	stuckWorkers := 0
	oldestHeartbeat := now
	var workerDetails []map[string]interface{}
	
	for workerID, lastSeen := range workerHeartbeats {
		timeSinceHeartbeat := now.Sub(lastSeen)
		isStuck := timeSinceHeartbeat > 60*time.Second // Consider stuck if no heartbeat for 1 minute
		
		if isStuck {
			stuckWorkers++
		} else {
			activeWorkers++
		}
		
		if lastSeen.Before(oldestHeartbeat) {
			oldestHeartbeat = lastSeen
		}
		
		workerDetails = append(workerDetails, map[string]interface{}{
			"worker_id":   workerID,
			"last_seen":   lastSeen.Format("15:04:05"),
			"idle_time":   timeSinceHeartbeat.Round(time.Second),
			"stuck":       isStuck,
		})
	}
	
	return map[string]interface{}{
		"total_workers":      len(workerHeartbeats),
		"active_workers":     activeWorkers,
		"stuck_workers":      stuckWorkers,
		"oldest_heartbeat":   now.Sub(oldestHeartbeat).Round(time.Second),
		"worker_details":     workerDetails,
	}
}

// Process job (streams to zstd/FS)
func ProcessJob(job models.Job, storage storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	// Simple: one browser per job, no sharing optimization
	return ProcessSingleJob(job, storage, db, archiversMap)
}

// REMOVED: ProcessCombinedBrowserJob - too complex, causing hangs
// Each job now gets its own fresh browser instance for maximum reliability

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
	
	// Create a multi-writer that writes to both database and stdout with job context
	stdoutPrefix := fmt.Sprintf("[%s-%s] ", job.ShortID, job.Type)
	prefixedStdout := &prefixWriter{prefix: stdoutPrefix}
	multiWriter := io.MultiWriter(dbLogWriter, prefixedStdout)
	
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
		data, ext, _, cleanup, archiveErr = arch.Archive(ctx, job.URL, multiWriter, db, item.ID)
		return archiveErr
	}, multiWriter, &currentRetryCount, retryConfig)
	
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
