package handlers

import (
	"fmt"
	"net/http"
	"time"
	"github.com/gin-gonic/gin"
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
		Pending    int64
		Processing int64
		Failed     int64
		QueueSize  int
	}
	db.Model(&models.ArchiveItem{}).Where("status = 'pending'").Count(&queueSummary.Pending)
	db.Model(&models.ArchiveItem{}).Where("status = 'processing'").Count(&queueSummary.Processing)
	db.Model(&models.ArchiveItem{}).Where("status = 'failed'").Count(&queueSummary.Failed)
	queueSummary.QueueSize = len(workers.JobChan)
	
	c.HTML(http.StatusOK, "admin.html", gin.H{
		"urls": urls,
		"queueSummary": queueSummary,
	})
}

func RequestCapture(c *gin.Context, db *gorm.DB) {
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
	shortID, err := workers.QueueCapture(db, u.ID, u.Original, types)
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
func RetryAllFailedJobs(c *gin.Context, db *gorm.DB) {
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
	queuedCount := 0
	
	for _, item := range failedItems {
		// Get the capture and URL information
		var capture models.Capture
		if err := db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			continue // Skip this item if we can't load capture info
		}
		
		// Reset the item status to pending and reset retry count for manual retry
		item.Status = "pending"
		item.RetryCount = 0 // Reset retry count for manual retry
		item.Logs = "Manual bulk retry at " + time.Now().Format("2006-01-02 15:04:05") + " (retry count reset)\n" + item.Logs
		
		if err := db.Save(&item).Error; err != nil {
			continue // Skip this item if we can't save
		}
		
		// Try to re-queue the job
		job := models.Job{
			CaptureID: item.CaptureID,
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}
		
		select {
		case workers.JobChan <- job:
			retriedCount++
		default:
			// Queue is full, but item is marked as pending so it will be picked up eventually
			queuedCount++
		}
	}
	
	var message string
	if retriedCount > 0 && queuedCount > 0 {
		message = fmt.Sprintf("Retried %d jobs immediately, %d jobs queued for retry", retriedCount, queuedCount)
	} else if retriedCount > 0 {
		message = fmt.Sprintf("Successfully retried %d failed jobs", retriedCount)
	} else if queuedCount > 0 {
		message = fmt.Sprintf("Queued %d jobs for retry (memory queue full)", queuedCount)
	} else {
		message = "No jobs were retried due to errors"
	}
	
	c.JSON(http.StatusOK, gin.H{"message": message})
}

func AdminArchive(c *gin.Context, db *gorm.DB) {
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
	
	shortID, err := workers.QueueCaptureForURL(db, req.URL, req.Types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}
	
	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)
	
	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}
