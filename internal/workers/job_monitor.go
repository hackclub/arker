package workers

import (
	"fmt"
	"log"
	"time"
	"gorm.io/gorm"
	"arker/internal/models"
)

// JobMonitor watches for stuck jobs and handles cleanup
type JobMonitor struct {
	db       *gorm.DB
	shutdown chan bool
	interval time.Duration
}

// NewJobMonitor creates a new job monitoring service
func NewJobMonitor(db *gorm.DB) *JobMonitor {
	return &JobMonitor{
		db:       db,
		shutdown: make(chan bool),
		interval: 5 * time.Minute, // Check every 5 minutes
	}
}

// Start begins monitoring for stuck jobs
func (jm *JobMonitor) Start() {
	log.Println("Starting job monitor...")
	go jm.monitorLoop()
}

// Stop stops the job monitor
func (jm *JobMonitor) Stop() {
	jm.shutdown <- true
}

// monitorLoop runs the monitoring checks
func (jm *JobMonitor) monitorLoop() {
	ticker := time.NewTicker(jm.interval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			jm.checkStuckJobs()
		case <-jm.shutdown:
			log.Println("Job monitor shutting down...")
			return
		}
	}
}

// checkStuckJobs identifies and handles jobs with no log updates in 5 minutes
func (jm *JobMonitor) checkStuckJobs() {
	log.Println("Checking for stuck jobs...")
	
	// Consider a job stuck if no log updates in 5 minutes
	stuckThreshold := 5 * time.Minute
	cutoffTime := time.Now().Add(-stuckThreshold)
	
	var stuckJobs []models.ArchiveItem
	err := jm.db.Where("status = 'processing' AND updated_at < ?", cutoffTime).
		Find(&stuckJobs).Error
	
	if err != nil {
		log.Printf("Error checking stuck jobs: %v", err)
		return
	}
	
	for _, job := range stuckJobs {
		jm.handleStuckJob(job, stuckThreshold)
	}
	
	// Also check for jobs that have been pending too long (possible queue overflow)
	jm.checkStaleQueuedJobs()
}

// handleStuckJob processes a single stuck job
func (jm *JobMonitor) handleStuckJob(job models.ArchiveItem, stuckThreshold time.Duration) {
	stuckDuration := time.Since(job.UpdatedAt)
	log.Printf("Found stuck job: ID=%d, Type=%s, NoUpdatesFor=%v", 
		job.ID, job.Type, stuckDuration)
	
	// Load capture information for logging
	var capture models.Capture
	if err := jm.db.Preload("ArchivedURL").First(&capture, job.CaptureID).Error; err != nil {
		log.Printf("Failed to load capture info for stuck job %d: %v", job.ID, err)
		return
	}
	
	// Mark job as failed with detailed explanation
	failureMessage := fmt.Sprintf(`
--- JOB STUCK DETECTED ---
Job had no log updates for %v (threshold: 5 minutes)
Started: %v
Last updated: %v
URL: %s
Type: %s
This typically indicates:
- Process hang or deadlock
- Network stall during download
- External service unavailability
- Server restart or process interruption

Marked as failed for automatic retry.
`, 
		stuckDuration,
		job.CreatedAt.Format("2006-01-02 15:04:05"),
		job.UpdatedAt.Format("2006-01-02 15:04:05"),
		capture.ArchivedURL.Original,
		job.Type,
	)
	
	// Update job status
	err := jm.db.Model(&job).Updates(map[string]interface{}{
		"status": "failed",
		"logs":   job.Logs + failureMessage,
	}).Error
	
	if err != nil {
		log.Printf("Failed to update stuck job %d: %v", job.ID, err)
		return
	}
	
	log.Printf("Marked stuck job %d as failed (type: %s, stuck for: %v)", 
		job.ID, job.Type, stuckDuration)
	
	// Optionally, could try to re-queue if under retry limit
	if job.RetryCount < 3 {
		jm.attemptRequeue(job, capture)
	}
}

// attemptRequeue tries to automatically requeue a failed job
func (jm *JobMonitor) attemptRequeue(job models.ArchiveItem, capture models.Capture) {
	// Wait a bit before retrying to avoid immediate re-failure
	time.Sleep(30 * time.Second)
	
	// Reset job to pending status
	job.Status = "pending"
	job.Logs += "\n--- AUTOMATIC RETRY AFTER TIMEOUT ---\n"
	
	if err := jm.db.Save(&job).Error; err != nil {
		log.Printf("Failed to reset job %d for retry: %v", job.ID, err)
		return
	}
	
	// Try to re-queue the job
	newJob := models.Job{
		CaptureID: job.CaptureID,
		ShortID:   capture.ShortID,
		Type:      job.Type,
		URL:       capture.ArchivedURL.Original,
	}
	
	select {
	case JobChan <- newJob:
		log.Printf("Successfully requeued stuck job %d", job.ID)
	default:
		// Queue is full - reset job to failed status since we can't queue it
		log.Printf("Queue full, marking job %d as failed (will retry on next monitor cycle)", job.ID)
		jm.db.Model(&job).Update("status", "failed")
	}
}

// checkStaleQueuedJobs looks for jobs that have been pending too long
func (jm *JobMonitor) checkStaleQueuedJobs() {
	// Jobs pending for more than 1 hour might indicate queue processing issues
	staleTime := time.Now().Add(-1 * time.Hour)
	
	var staleJobs []models.ArchiveItem
	err := jm.db.Where("status = 'pending' AND created_at < ?", staleTime).
		Find(&staleJobs).Error
	
	if err != nil {
		log.Printf("Error checking stale queued jobs: %v", err)
		return
	}
	
	if len(staleJobs) > 0 {
		log.Printf("Warning: Found %d jobs that have been pending for over 1 hour", len(staleJobs))
		
		// Could implement auto-retry logic here or just log for admin attention
		for _, job := range staleJobs {
			if len(staleJobs) <= 5 { // Only log details for small numbers to avoid spam
				log.Printf("Stale job: ID=%d, Type=%s, Pending since=%v", 
					job.ID, job.Type, job.CreatedAt.Format("2006-01-02 15:04:05"))
			}
		}
	}
}


