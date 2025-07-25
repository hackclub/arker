package handlers

import (
	"net/http"
	"time"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/utils"
)

func DisplayGet(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).Preload("ArchiveItems").First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Get the original URL for the capture
	var archivedURL models.ArchivedURL
	db.First(&archivedURL, capture.ArchivedURLID)
	
	// Check if this is a git repository and generate clone info
	isGit := utils.IsGitURL(archivedURL.Original)
	var gitRepoName string
	if isGit {
		gitRepoName = utils.ExtractRepoName(archivedURL.Original)
	}
	
	c.HTML(http.StatusOK, "display.html", gin.H{
		"date":          capture.Timestamp.Format(time.RFC1123),
		"timestamp":     capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"archives":      capture.ArchiveItems,
		"short_id":      shortID,
		"host":          c.Request.Host,
		"original_url":  archivedURL.Original,
		"is_git":        isGit,
		"git_repo_name": gitRepoName,
	})
}

func GetLogs(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	typ := c.Param("type")
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, typ).
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": item.Logs, "status": item.Status})
}
