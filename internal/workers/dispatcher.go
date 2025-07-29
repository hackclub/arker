package workers

import (
	"log"
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
	log.Println("Starting job dispatcher...")
	go d.dispatchLoop()
}

// Stop stops the dispatcher
func (d *Dispatcher) Stop() {
	d.shutdown <- true
}

// dispatchLoop continuously polls for pending jobs
func (d *Dispatcher) dispatchLoop() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			d.dispatchPendingJobs()
		case <-d.shutdown:
			log.Println("Job dispatcher shutting down...")
			return
		}
	}
}

// dispatchPendingJobs finds pending jobs and sends them to workers
func (d *Dispatcher) dispatchPendingJobs() {
	// First, handle failed jobs that should be retried (failed > 30 seconds ago)
	retryTime := time.Now().Add(-30 * time.Second)
	d.db.Model(&models.ArchiveItem{}).
		Where("status = 'failed' AND retry_count < ? AND updated_at < ?", 3, retryTime).
		Update("status", "pending")
	
	// Also handle stuck processing jobs (processing > 5 minutes)
	stuckTime := time.Now().Add(-5 * time.Minute)
	d.db.Model(&models.ArchiveItem{}).
		Where("status = 'processing' AND updated_at < ?", stuckTime).
		Update("status", "failed")
	
	var pendingItems []models.ArchiveItem
	err := d.db.Where("status = 'pending' AND retry_count < ?", 3).
		Order("created_at ASC").
		Limit(50). // Process up to 50 at a time
		Find(&pendingItems).Error
	
	if err != nil {
		log.Printf("Error fetching pending jobs: %v", err)
		return
	}
	
	if len(pendingItems) == 0 {
		return // No pending jobs
	}
	
	log.Printf("Dispatching %d pending jobs...", len(pendingItems))
	
	for _, item := range pendingItems {
		// Load capture and URL info
		var capture models.Capture
		if err := d.db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			log.Printf("Failed to load capture %d: %v", item.CaptureID, err)
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
			log.Printf("Dispatched job: %s %s", job.ShortID, job.Type)
		default:
			// Channel is full, workers are busy - we'll try again next cycle
			log.Printf("Worker channel full, will retry job %s %s next cycle", job.ShortID, job.Type)
			return // Don't overwhelm, try again in 2 seconds
		}
	}
}
