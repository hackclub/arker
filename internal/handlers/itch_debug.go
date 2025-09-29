package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ServeItchDebug is a minimal debug version to test basic functionality
func ServeItchDebug(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	
	// Test the exact database query our main handler uses
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "itch").
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "archive_not_found",
			"shortid": shortID,
			"query_error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"debug": "database_query_success",
		"shortid": shortID,
		"item_id": item.ID,
		"status": item.Status,
		"storage_key": item.StorageKey,
	})
}
