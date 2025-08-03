package workers

import (
	"log/slog"
	"time"
	"gorm.io/gorm"
	"arker/internal/models"
)

// Dispatcher continuously polls database for pending jobs and feeds workers
type Dispatcher struct {
	db       *gorm.DB
	jobChan  chan models.Job
	interval time.Duration
	shutdown chan bool
}

// NewDispatcher creates a new job dispatcher
func NewDispatcher(db *gorm.DB, jobChan chan models.Job) *Dispatcher {
	return &Dispatcher{
		db:       db,
		jobChan:  jobChan,
		interval: 2 * time.Second, // Poll every 2 seconds
		shutdown: make(chan bool),
	}
}

// Start begins the dispatcher loop
func (d *Dispatcher) Start() {
	slog.Info("Starting job dispatcher", 
		"poll_interval", d.interval,
		"channel_capacity", cap(d.jobChan))
	go d.dispatchLoop()
}

// Stop stops the dispatcher
func (d *Dispatcher) Stop() {
	slog.Info("Stopping job dispatcher")
	d.shutdown <- true
}

// dispatchLoop continuously polls for pending jobs
func (d *Dispatcher) dispatchLoop() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	
	slog.Info("Job dispatcher loop started")
	
	for {
		select {
		case <-ticker.C:
			d.dispatchPendingJobs()
		case <-d.shutdown:
			slog.Info("Job dispatcher shutting down")
			return
		}
	}
}

// dispatchPendingJobs finds pending jobs and sends them to workers
func (d *Dispatcher) dispatchPendingJobs() {
	startTime := time.Now()
	
	// First, handle failed jobs that should be retried (failed > 30 seconds ago)
	// Update created_at to current time so retries go to back of queue
	retryTime := time.Now().Add(-30 * time.Second)
	currentTime := time.Now()
	retryResult := d.db.Model(&models.ArchiveItem{}).
		Where("status = 'failed' AND retry_count < ? AND updated_at < ?", 3, retryTime).
		Updates(map[string]interface{}{
			"status": "pending",
			"created_at": currentTime,
		})
	
	if retryResult.RowsAffected > 0 {
		slog.Info("Auto-retrying failed jobs",
			"count", retryResult.RowsAffected,
			"retry_delay", "30s")
	}
	
	// Also handle stuck processing jobs (processing > 5 minutes)
	stuckTime := time.Now().Add(-5 * time.Minute)
	stuckResult := d.db.Model(&models.ArchiveItem{}).
		Where("status = 'processing' AND updated_at < ?", stuckTime).
		Update("status", "failed")
	
	if stuckResult.RowsAffected > 0 {
		slog.Warn("Marked stuck processing jobs as failed",
			"count", stuckResult.RowsAffected,
			"stuck_threshold", "5m")
	}
	
	// Get queue statistics for logging
	var queueStats struct {
		Pending    int64
		Processing int64
		Failed     int64
		Completed  int64
	}
	d.db.Model(&models.ArchiveItem{}).Where("status = 'pending'").Count(&queueStats.Pending)
	d.db.Model(&models.ArchiveItem{}).Where("status = 'processing'").Count(&queueStats.Processing)
	d.db.Model(&models.ArchiveItem{}).Where("status = 'failed'").Count(&queueStats.Failed)
	d.db.Model(&models.ArchiveItem{}).Where("status = 'completed'").Count(&queueStats.Completed)
	
	var pendingItems []models.ArchiveItem
	err := d.db.Where("status = 'pending' AND retry_count < ?", 3).
		Order("created_at ASC").
		Limit(50). // Process up to 50 at a time
		Find(&pendingItems).Error
	
	if err != nil {
		slog.Error("Error fetching pending jobs", "error", err)
		return
	}
	
	// Log queue state every 10 cycles (20 seconds) or if there are jobs to dispatch
	if len(pendingItems) > 0 || time.Now().Unix()%20 == 0 {
		slog.Info("Queue status",
			"pending", queueStats.Pending,
			"processing", queueStats.Processing,
			"failed", queueStats.Failed,
			"completed", queueStats.Completed,
			"worker_channel_used", len(d.jobChan),
			"worker_channel_capacity", cap(d.jobChan))
	}
	
	if len(pendingItems) == 0 {
		return // No pending jobs to dispatch
	}
	
	slog.Info("Dispatching pending jobs",
		"count", len(pendingItems),
		"channel_available", cap(d.jobChan)-len(d.jobChan))
	
	dispatched := 0
	skipped := 0
	
	for _, item := range pendingItems {
		// Load capture and URL info
		var capture models.Capture
		if err := d.db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			slog.Error("Failed to load capture for job",
				"capture_id", item.CaptureID,
				"item_id", item.ID,
				"error", err)
			skipped++
			continue
		}
		
		// Create job
		job := models.Job{
			CaptureID: capture.ID,
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}
		
		// Try to send to worker channel (non-blocking)
		select {
		case d.jobChan <- job:
			slog.Debug("Dispatched job to worker",
				"short_id", job.ShortID,
				"type", job.Type,
				"url", job.URL,
				"retry_count", item.RetryCount,
				"age", time.Since(item.CreatedAt).Round(time.Second))
			dispatched++
		default:
			// Channel is full, workers are busy - investigate why
			slog.Warn("Worker channel full - workers may be stuck",
				"first_blocked_job", job.ShortID,
				"first_blocked_type", job.Type,
				"channel_used", len(d.jobChan),
				"channel_capacity", cap(d.jobChan),
				"deferred_count", len(pendingItems)-dispatched,
				"pending_types", d.getPendingJobTypes(pendingItems))
			
			// Log details about stuck processing jobs and worker status
			d.logStuckProcessingJobs()
			d.logWorkerStatus()
			break // Exit loop, try again in 2 seconds
		}
	}
	
	if dispatched > 0 || skipped > 0 {
		slog.Info("Dispatch cycle completed",
			"dispatched", dispatched,
			"skipped", skipped,
			"duration", time.Since(startTime).Round(time.Millisecond))
	}
}

// getPendingJobTypes returns a summary of job types being dispatched
func (d *Dispatcher) getPendingJobTypes(items []models.ArchiveItem) map[string]int {
	typeCount := make(map[string]int)
	for _, item := range items {
		typeCount[item.Type]++
	}
	return typeCount
}

// logStuckProcessingJobs logs details about jobs that have been processing too long
func (d *Dispatcher) logStuckProcessingJobs() {
	var processingJobs []struct {
		ID        uint
		Type      string
		ShortID   string
		URL       string
		UpdatedAt time.Time
	}
	
	// Get processing jobs with capture info
	d.db.Table("archive_items").
		Select("archive_items.id, archive_items.type, captures.short_id, archived_urls.original as url, archive_items.updated_at").
		Joins("JOIN captures ON archive_items.capture_id = captures.id").
		Joins("JOIN archived_urls ON captures.archived_url_id = archived_urls.id").
		Where("archive_items.status = 'processing'").
		Order("archive_items.updated_at ASC").
		Limit(10).
		Find(&processingJobs)
	
	if len(processingJobs) > 0 {
		slog.Warn("Found stuck processing jobs",
			"count", len(processingJobs),
			"oldest_job", processingJobs[0].ShortID,
			"oldest_type", processingJobs[0].Type,
			"oldest_duration", time.Since(processingJobs[0].UpdatedAt).Round(time.Second))
		
		// Log details for each stuck job
		for _, job := range processingJobs {
			slog.Warn("Stuck processing job details",
				"short_id", job.ShortID,
				"type", job.Type,
				"url", job.URL,
				"stuck_duration", time.Since(job.UpdatedAt).Round(time.Second),
				"last_update", job.UpdatedAt.Format("15:04:05"))
		}
	}
}

// logWorkerStatus logs the current status of all workers
func (d *Dispatcher) logWorkerStatus() {
	status := GetWorkerStatus()
	
	slog.Warn("Worker status during channel congestion",
		"total_workers", status["total_workers"],
		"active_workers", status["active_workers"],
		"stuck_workers", status["stuck_workers"],
		"oldest_heartbeat", status["oldest_heartbeat"])
	
	// Log details for each worker
	if workerDetails, ok := status["worker_details"].([]map[string]interface{}); ok {
		for _, worker := range workerDetails {
			if stuck, ok := worker["stuck"].(bool); ok && stuck {
				slog.Error("Worker appears stuck",
					"worker_id", worker["worker_id"],
					"last_seen", worker["last_seen"],
					"idle_time", worker["idle_time"])
			} else {
				slog.Debug("Worker status",
					"worker_id", worker["worker_id"],
					"last_seen", worker["last_seen"],
					"idle_time", worker["idle_time"])
			}
		}
	}
}
