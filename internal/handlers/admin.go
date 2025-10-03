package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"arker/internal/workers"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"
)

// MigrateZstdKeys removes .zst suffix from storage keys and recomputes file sizes
func MigrateZstdKeys(c *gin.Context, db *gorm.DB, storage storage.Storage) {
	if !RequireLogin(c) {
		return
	}

	var items []models.ArchiveItem
	result := db.Where("storage_key LIKE '%.zst'").Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to find items with .zst keys"})
		return
	}

	updated := 0
	failed := 0

	for _, item := range items {
		// Remove .zst suffix
		newKey := strings.TrimSuffix(item.StorageKey, ".zst")

		// Check if the new key exists in storage
		exists, err := storage.Exists(newKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to check storage for %s: %v", newKey, err)})
			return
		}

		if !exists {
			failed++
			continue
		}

		// Get new file size
		newSize, err := storage.Size(newKey)
		if err != nil {
			failed++
			continue
		}

		// Update database
		if err := db.Model(&item).Updates(map[string]interface{}{
			"storage_key": newKey,
			"file_size":   newSize,
		}).Error; err != nil {
			failed++
			continue
		}

		updated++
	}

	c.JSON(http.StatusOK, gin.H{
		"message":     "Migration completed",
		"total_found": len(items),
		"updated":     updated,
		"failed":      failed,
	})
}

func AdminGet(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	// Get pagination parameters
	page := 1
	if p := c.Query("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}

	// Get search parameter
	search := c.Query("search")

	const limit = 1000
	offset := (page - 1) * limit

	// Build base query with search filter
	baseQuery := db.Model(&models.ArchivedURL{}).
		Joins("LEFT JOIN captures ON archived_urls.id = captures.archived_url_id").
		Joins("LEFT JOIN archive_items ON captures.id = archive_items.capture_id").
		Group("archived_urls.id")

	if search != "" {
		baseQuery = baseQuery.Where("archived_urls.original ILIKE ?", "%"+search+"%")
	}

	// Get total count for pagination info
	var total int64
	baseQuery.Count(&total)

	// Build query for fetching URLs with same search filter
	urlQuery := db.Preload("Captures.ArchiveItems").Preload("Captures.APIKey").Preload("Captures", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at DESC")
	}).Joins("LEFT JOIN captures ON archived_urls.id = captures.archived_url_id").
		Joins("LEFT JOIN archive_items ON captures.id = archive_items.capture_id").
		Group("archived_urls.id").
		Order("MAX(archive_items.created_at) DESC").
		Offset(offset).Limit(limit)

	if search != "" {
		urlQuery = urlQuery.Where("archived_urls.original ILIKE ?", "%"+search+"%")
	}

	var urls []models.ArchivedURL
	urlQuery.Find(&urls)

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

	// Calculate pagination info
	totalPages := int(math.Ceil(float64(total) / float64(limit)))

	c.HTML(http.StatusOK, "admin.html", gin.H{
		"urls":         urls,
		"queueSummary": queueSummary,
		"search":       search,
		"pagination": gin.H{
			"currentPage": page,
			"totalPages":  totalPages,
			"totalItems":  total,
			"hasNext":     page < totalPages,
			"hasPrev":     page > 1,
			"nextPage":    page + 1,
			"prevPage":    page - 1,
		},
	})
}

func RequestCapture(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
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
	shortID, err := workers.QueueCapture(c.Request.Context(), db, riverClient, u.Original, types, nil)
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

// RetryAllFailedJobs directly retries all failed archive items
func RetryAllFailedJobs(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	if !RequireLogin(c) {
		return
	}

	// Get all failed items
	var items []models.ArchiveItem
	if err := db.Where("status = 'failed'").Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get failed items"})
		return
	}

	if len(items) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No failed jobs to retry"})
		return
	}

	// Reset status to pending and enqueue new jobs
	retriedCount := 0
	for _, item := range items {
		// Get the capture info for this item
		var capture models.Capture
		if err := db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			continue // Skip if we can't find the capture
		}

		// Update status to pending
		if err := db.Model(&item).Update("status", "pending").Error; err != nil {
			continue // Skip this item if update fails
		}

		// Queue new archive job
		args := workers.ArchiveJobArgs{
			CaptureID: 0, // Will be looked up by short_id and type
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}

		opts := &river.InsertOpts{
			MaxAttempts: 3,
			Tags:        []string{"archive", item.Type, "retry"},
			UniqueOpts: river.UniqueOpts{
				ByArgs:   true,
				ByPeriod: 1 * time.Minute,
			},
		}

		if _, err := riverClient.Insert(c.Request.Context(), args, opts); err == nil {
			retriedCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Retried %d of %d failed items", retriedCount, len(items)),
	})
}

func AdminArchive(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
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

	shortID, err := workers.QueueCapture(c.Request.Context(), db, riverClient, req.URL, req.Types, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}

	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)

	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}

// MigrateItchArchives deletes old itch archives and marks them as failed for retry
func MigrateItchArchives(c *gin.Context, db *gorm.DB, storageInstance storage.Storage) {
	if !RequireLogin(c) {
		return
	}

	// Find all itch archive items
	var items []models.ArchiveItem
	result := db.Where("type = ?", "itch").Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to find itch archive items"})
		return
	}

	deleted := 0
	markedFailed := 0
	failed := 0

	for _, item := range items {
		// Delete the storage file directly
		// Note: This assumes FSStorage - for other storage types this would need to be adapted
		if fsStorage, ok := storageInstance.(*storage.FSStorage); ok {
			filePath := filepath.Join(fsStorage.BaseDir(), item.StorageKey)
			if err := os.Remove(filePath); err != nil {
				fmt.Printf("Warning: Failed to delete storage file %s: %v\n", item.StorageKey, err)
				failed++
			} else {
				deleted++
			}
		} else {
			// For non-FS storage, just mark as failed without deleting the file
			deleted++
		}

		// Mark the archive item as failed so it can be retried
		updateResult := db.Model(&item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   fmt.Sprintf("Marked as failed during itch migration - will be retried with new format (%s)", time.Now().Format(time.RFC3339)),
		})
		if updateResult.Error != nil {
			fmt.Printf("Warning: Failed to mark item %d as failed: %v\n", item.ID, updateResult.Error)
			failed++
		} else {
			markedFailed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":        "Itch migration completed",
		"total_items":    len(items),
		"deleted_files":  deleted,
		"marked_failed":  markedFailed,
		"failed_updates": failed,
	})
}
