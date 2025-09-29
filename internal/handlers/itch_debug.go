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
	
	// Get URL info for a more realistic metadata response
	var capture models.Capture
	db.Where("short_id = ?", shortID).First(&capture)
	
	var archivedURL models.ArchivedURL
	db.First(&archivedURL, capture.ArchivedURLID)
	
	// Return realistic metadata for UI testing
	c.JSON(http.StatusOK, gin.H{
		"title": "Archived Game",
		"url": archivedURL.Original,
		"author": "Developer",
		"description": "This game has been archived from itch.io. Individual file serving is temporarily unavailable for large archives.",
		"platforms": []string{"Windows"},
		"is_web_game": false,
		"game_files": []gin.H{
			{
				"name": "game.zip",
				"platform": "Archive",
				"size": item.FileSize,
			},
		},
	})
}
