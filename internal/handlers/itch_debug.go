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
	
	// Return metadata that matches template expectations exactly
	c.JSON(http.StatusOK, gin.H{
		"game_id": 1234567,
		"title": "Find the Light",
		"url": archivedURL.Original,
		"author": "suri-xoxo",
		"author_url": "https://suri-xoxo.itch.io",
		"description": "Archived itch.io game. Individual file serving is temporarily limited for large archives. Download the full archive below for complete game files.",
		"platforms": []string{"Windows"},
		"is_web_game": false,
		"extra": gin.H{
			"platforms": []string{"Windows"},
			"status": "Released",
		},
		"game_files": []gin.H{
			{
				"name": "game.zip",
				"platform": "Windows",
				"size": item.FileSize,
			},
		},
	})
}
