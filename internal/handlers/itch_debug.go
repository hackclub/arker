package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ServeItchDebug is a minimal debug version to test basic functionality
func ServeItchDebug(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	
	c.Header("X-Debug-Route", "itch-debug")
	c.Header("X-Debug-ShortID", shortID)
	
	// Test database lookup
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "itch").
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "archive not found",
			"shortid": shortID,
			"db_error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"status": "debug success",
		"shortid": shortID,
		"item_id": item.ID,
		"item_status": item.Status,
		"storage_key": item.StorageKey,
		"file_size": item.FileSize,
	})
}
