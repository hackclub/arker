package handlers

import (
	"fmt"
	"net/http"
	"time"
	"github.com/gin-gonic/gin"
	"github.com/riverqueue/river"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/utils"
	"arker/internal/workers"
)

func AdminGet(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}
	var urls []models.ArchivedURL
	// Sort by most recent archive creation (not URL creation)
	db.Preload("Captures.ArchiveItems").Preload("Captures.APIKey").Preload("Captures", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at DESC")
	}).Joins("LEFT JOIN captures ON archived_urls.id = captures.archived_url_id").
		Joins("LEFT JOIN archive_items ON captures.id = archive_items.capture_id").
		Group("archived_urls.id").
		Order("MAX(archive_items.created_at) DESC").Find(&urls)
	
	// Add queue status summary for dashboard
	var queueSummary struct {
		Pending         int64
		Processing      int64
		Failed          int64
		QueueSize       int
		RecentCompleted int64
	}
	db.Model(&models.ArchiveItem{}).Where("status = 'pending'").Count(&queueSummary.Pending)
	db.Model(&models.ArchiveItem{}).Where("status = 'processing'").Count(&queueSummary.Processing)
	db.Model(&models.ArchiveItem{}).Where("status = 'failed'").Count(&queueSummary.Failed)
	// River queue size is managed internally, we can get stats if needed
	queueSummary.QueueSize = 0 // River handles queue internally
	
	// Count jobs completed in the past 5 minutes
	fiveMinutesAgo := time.Now().Add(-5 * time.Minute)
	db.Model(&models.ArchiveItem{}).Where("status = 'completed' AND updated_at > ?", fiveMinutesAgo).Count(&queueSummary.RecentCompleted)
	

	
	c.HTML(http.StatusOK, "admin.html", gin.H{
		"urls": urls,
		"queueSummary": queueSummary,

	})
}

func RequestCapture(c *gin.Context, db *gorm.DB, riverQueueManager *workers.RiverQueueManager) {
	if !RequireLogin(c) {
		return
	}
	id := c.Param("id")
	var u models.ArchivedURL
	if db.First(&u, id).Error != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL ID"})
		return
	}
	
	types := utils.GetArchiveTypes(u.Original)
	shortID, err := riverQueueManager.QueueCapture(u.ID, u.Original, types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}
	
	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)
	
	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}

func GetItemLog(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) { 
		return 
	}
	id := c.Param("id")
	var item models.ArchiveItem
	if db.First(&item, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": item.Logs})
}

// RetryAllFailedJobs queues a background job to retry all failed archive items
func RetryAllFailedJobs(c *gin.Context, db *gorm.DB, riverQueueManager *workers.RiverQueueManager) {
	if !RequireLogin(c) {
		return
	}
	
	// Check if there are any failed items
	var failedCount int64
	if err := db.Model(&models.ArchiveItem{}).Where("status = 'failed'").Count(&failedCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to count failed items"})
		return
	}
	
	if failedCount == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No failed jobs to retry"})
		return
	}
	
	// Queue a background job to handle the bulk retry
	args := workers.BulkRetryJobArgs{
		RequestedBy: "admin", // Could be enhanced to track which admin user
	}
	
	opts := &river.InsertOpts{
		MaxAttempts: 1, // Bulk retry jobs shouldn't themselves be retried
		Queue:       "high_priority", // Use high-priority queue for faster processing
		Tags:        []string{"bulk_retry", "admin"},
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 5 * time.Minute, // Prevent multiple bulk retries in short succession
		},
	}
	
	if _, err := riverQueueManager.RiverClient.Insert(c.Request.Context(), args, opts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue bulk retry job"})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Bulk retry job queued to process %d failed jobs in the background", failedCount),
	})
}

func AdminArchive(c *gin.Context, db *gorm.DB, riverQueueManager *workers.RiverQueueManager) {
	if !RequireLogin(c) {
		return
	}

	var req utils.ArchiveRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}
	
	// Validate the request including SSRF protection
	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	
	shortID, err := riverQueueManager.QueueCaptureForURL(req.URL, req.Types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}
	
	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)
	
	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}
