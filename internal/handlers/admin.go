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
	queueSummary.QueueSize = len(workers.JobChan)
	
	// Count jobs completed in the past 5 minutes
	fiveMinutesAgo := time.Now().Add(-5 * time.Minute)
	db.Model(&models.ArchiveItem{}).Where("status = 'completed' AND updated_at > ?", fiveMinutesAgo).Count(&queueSummary.RecentCompleted)
	
	// Get SOCKS proxy health status
	socksStatus := utils.GetSOCKSHealthChecker().GetStatus()
	
	c.HTML(http.StatusOK, "admin.html", gin.H{
		"urls": urls,
		"queueSummary": queueSummary,
		"socksStatus": socksStatus,
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

// RetryAllFailedJobs retries all failed archive items
func RetryAllFailedJobs(c *gin.Context, db *gorm.DB, riverQueueManager *workers.RiverQueueManager) {
	if !RequireLogin(c) {
		return
	}
	
	// Get all failed items with their capture information
	var failedItems []models.ArchiveItem
	if err := db.Where("status = 'failed'").Find(&failedItems).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to find failed items"})
		return
	}
	
	if len(failedItems) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No failed jobs to retry"})
		return
	}
	
	retriedCount := 0
	
	for _, item := range failedItems {
		// Get the capture and URL information
		var capture models.Capture
		if err := db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			continue // Skip this item if we can't load capture info
		}
		
		// Reset the item status to pending and reset retry count for manual retry
		currentTime := time.Now()
		item.Status = "pending"
		item.RetryCount = 0 // Reset retry count for manual retry
		item.Logs = "Manual bulk retry at " + currentTime.Format("2006-01-02 15:04:05") + " (retry count reset)\n" + item.Logs
		
		if err := db.Save(&item).Error; err != nil {
			continue // Skip this item if we can't save
		}
		
		// Enqueue the job in River
		args := workers.ArchiveJobArgs{
			CaptureID: capture.ID,
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}
		
		opts := &river.InsertOpts{
			MaxAttempts: 3,
			Tags:        []string{"archive", item.Type, "retry"},
		}
		
		if _, err := riverQueueManager.RiverClient.Insert(c.Request.Context(), args, opts); err != nil {
			// If enqueueing fails, mark item as failed again
			item.Status = "failed"
			item.Logs = item.Logs + "\nFailed to enqueue retry in River: " + err.Error()
			db.Save(&item)
			continue
		}
		
		retriedCount++
	}
	
	var message string
	if retriedCount > 0 {
		message = fmt.Sprintf("Successfully queued %d failed jobs for retry", retriedCount)
	} else {
		message = "No jobs were retried due to errors"
	}
	
	c.JSON(http.StatusOK, gin.H{"message": message})
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
