package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
	"arker/internal/workers"
)

// PastArchiveResponse defines the structure for a past archive entry.
type PastArchiveResponse struct {
	ShortID   string    `json:"short_id"`
	Timestamp time.Time `json:"timestamp"`
	// ThumbnailURL always points at a servable image. The endpoint falls back
	// to a placeholder, so consumers can render a card unconditionally instead
	// of branching on whether a preview happens to exist yet.
	ThumbnailURL string `json:"thumbnail_url"`
}

// getPastArchives is the shared logic for retrieving past archives.
func getPastArchives(c *gin.Context, db *gorm.DB) {
	url := c.Query("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url parameter is required"})
		return
	}

	if err := utils.ValidateURL(url); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL: " + err.Error()})
		return
	}

	var archivedURL models.ArchivedURL
	if err := db.Where("original = ?", url).First(&archivedURL).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusOK, []PastArchiveResponse{}) // Return empty array
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	var captures []models.Capture
	db.Where("archived_url_id = ?", archivedURL.ID).Preload("ArchiveItems").Order("created_at DESC").Limit(10).Find(&captures)

	response := make([]PastArchiveResponse, len(captures))
	for i, capture := range captures {
		thumbnailKey := ""
		if ready, _ := selectThumbnailItem(capture.ArchiveItems, defaultTypePreference(archivedURL.Original)); ready != nil {
			thumbnailKey = ready.ThumbnailKey
		}
		response[i] = PastArchiveResponse{
			ShortID:      capture.ShortID,
			Timestamp:    capture.Timestamp,
			ThumbnailURL: ThumbnailURL(c, capture.ShortID, thumbnailKey),
		}
	}

	c.JSON(http.StatusOK, response)
}

// ApiPastArchives is the API-key-protected handler.
func ApiPastArchives(c *gin.Context, db *gorm.DB) {
	getPastArchives(c, db)
}

// WebPastArchives is the public handler for the web interface.
func WebPastArchives(c *gin.Context, db *gorm.DB) {
	getPastArchives(c, db)
}

// ApiArchive handles new archive requests from the API.
func ApiArchive(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	var req utils.ArchiveRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	apiKey, _ := c.Get("api_key")
	apiKeyID := apiKey.(*models.APIKey).ID

	shortID, err := workers.QueueCapture(c.Request.Context(), db, riverClient, req.URL, req.Types, &apiKeyID, req.Force)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}

	c.JSON(http.StatusOK, archiveQueuedResponse(c, shortID))
}

func archiveQueuedResponse(c *gin.Context, shortID string) gin.H {
	return gin.H{
		"url":        utils.BuildFullURL(c, shortID),
		"short_id":   shortID,
		"result_url": utils.BuildFullURL(c, "api/v1/archive/"+shortID),
	}
}

// ApiFindOrCreateArchive returns a reusable canonical archive, joins work
// already underway, or starts a new capture.
func ApiFindOrCreateArchive(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	var req utils.ArchiveRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}
	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	apiKey, ok := c.Get("api_key")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
		return
	}
	apiKeyID := apiKey.(*models.APIKey).ID
	result, err := workers.FindOrCreateCapture(c.Request.Context(), db, riverClient, req.URL, req.Types, &apiKeyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to find or create capture"})
		return
	}

	resultURL := utils.BuildFullURL(c, "api/v1/archive/"+result.ShortID)
	c.Header("Location", resultURL)
	statusCode := http.StatusAccepted
	if result.Action == workers.FindOrCreateFound {
		statusCode = http.StatusOK
	}
	c.JSON(statusCode, gin.H{
		"action":     result.Action,
		"short_id":   result.ShortID,
		"result_url": resultURL,
		"status":     result.Status,
	})
}
