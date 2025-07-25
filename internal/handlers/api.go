package handlers

import (
	"net/http"
	"time"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/workers"
)

func ApiPastArchives(c *gin.Context, db *gorm.DB) {
	url := c.Query("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url parameter is required"})
		return
	}

	var archivedURL models.ArchivedURL
	if err := db.Where("original = ?", url).First(&archivedURL).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusOK, []interface{}{})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	var captures []models.Capture
	if err := db.Where("archived_url_id = ?", archivedURL.ID).
		Order("created_at DESC").
		Limit(10).
		Find(&captures).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	type PastArchive struct {
		ShortID   string    `json:"short_id"`
		Timestamp time.Time `json:"timestamp"`
	}

	var pastArchives []PastArchive
	for _, capture := range captures {
		pastArchives = append(pastArchives, PastArchive{
			ShortID:   capture.ShortID,
			Timestamp: capture.Timestamp,
		})
	}

	c.JSON(http.StatusOK, pastArchives)
}

func ApiArchive(c *gin.Context, db *gorm.DB) {
	var req struct {
		URL   string   `json:"url"`
		Types []string `json:"types"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	
	shortID, err := workers.QueueCaptureForURL(db, req.URL, req.Types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{"short_id": shortID})
}
