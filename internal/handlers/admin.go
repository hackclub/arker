package handlers

import (
	"net/http"
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
	c.HTML(http.StatusOK, "admin.html", gin.H{"urls": urls})
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
	
	c.JSON(http.StatusOK, gin.H{"short_id": shortID})
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

func AdminArchive(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	
	shortID, err := workers.QueueCaptureForURL(db, req.URL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{"short_id": shortID})
}
