package handlers

import (
	"net/http"
	"time"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/utils"
)

// URL type mapping: user-facing URLs use "web" instead of "mhtml"
func urlTypeToInternalType(urlType string) string {
	if urlType == "web" {
		return "mhtml"
	}
	return urlType
}

func internalTypeToURLType(internalType string) string {
	if internalType == "mhtml" {
		return "web"
	}
	return internalType
}

func getDisplayName(internalType string) string {
	if internalType == "mhtml" {
		return "Web"
	}
	return internalType
}

// DisplayDefault serves the default archive type view directly (no redirect)
func DisplayDefault(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).Preload("ArchiveItems").First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Get the original URL for the capture
	var archivedURL models.ArchivedURL
	db.First(&archivedURL, capture.ArchivedURLID)
	
	// Determine the default archive type based on URL type
	isGit := utils.IsGitURL(archivedURL.Original)
	isYouTube := utils.IsYouTubeURL(archivedURL.Original)
	var defaultType string
	
	if isGit {
		// For git repositories, prefer git -> mhtml -> screenshot -> youtube
		for _, preferredType := range []string{"git", "mhtml", "screenshot", "youtube"} {
			for _, item := range capture.ArchiveItems {
				if item.Type == preferredType {
					defaultType = preferredType
					break
				}
			}
			if defaultType != "" {
				break
			}
		}
	} else if isYouTube {
		// For YouTube URLs, prefer youtube -> mhtml -> screenshot -> git
		for _, preferredType := range []string{"youtube", "mhtml", "screenshot", "git"} {
			for _, item := range capture.ArchiveItems {
				if item.Type == preferredType {
					defaultType = preferredType
					break
				}
			}
			if defaultType != "" {
				break
			}
		}
	} else {
		// For websites, prefer mhtml -> screenshot -> git -> youtube
		for _, preferredType := range []string{"mhtml", "screenshot", "git", "youtube"} {
			for _, item := range capture.ArchiveItems {
				if item.Type == preferredType {
					defaultType = preferredType
					break
				}
			}
			if defaultType != "" {
				break
			}
		}
	}
	
	// If no preferred type found, use the first available
	if defaultType == "" && len(capture.ArchiveItems) > 0 {
		defaultType = capture.ArchiveItems[0].Type
	}
	
	if defaultType == "" {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Find the specific archive item
	var targetItem *models.ArchiveItem
	for i := range capture.ArchiveItems {
		if capture.ArchiveItems[i].Type == defaultType {
			targetItem = &capture.ArchiveItems[i]
			break
		}
	}
	
	if targetItem == nil {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Check if this is a git repository and generate clone info
	var gitRepoName string
	if isGit {
		gitRepoName = utils.ExtractRepoName(archivedURL.Original)
	}
	
	// Generate filename for downloads
	filename := utils.GenerateArchiveFilename(capture, archivedURL, targetItem.Extension)
	
	// Serve the default archive type view directly
	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":          capture.Timestamp.Format(time.RFC1123),
		"timestamp":     capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"archives":      capture.ArchiveItems,
		"current_item":  targetItem,
		"current_type":  internalTypeToURLType(defaultType), // Convert to URL type for display
		"short_id":      shortID,
		"host":          c.Request.Host,
		"original_url":  archivedURL.Original,
		"is_git":        isGit,
		"is_youtube":    isYouTube,
		"git_repo_name": gitRepoName,
		"download_filename": filename,
	})
}

// DisplayType shows a specific archive type page
func DisplayType(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	urlType := c.Param("type")
	
	// Convert URL type to internal type for database lookup
	internalType := urlTypeToInternalType(urlType)
	
	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).Preload("ArchiveItems").First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Get the original URL for the capture
	var archivedURL models.ArchivedURL
	db.First(&archivedURL, capture.ArchivedURLID)
	
	// Find the specific archive item using internal type
	var targetItem *models.ArchiveItem
	for i := range capture.ArchiveItems {
		if capture.ArchiveItems[i].Type == internalType {
			targetItem = &capture.ArchiveItems[i]
			break
		}
	}
	
	if targetItem == nil {
		c.Status(http.StatusNotFound)
		return
	}
	
	// Check if this is a git repository and generate clone info
	isGit := utils.IsGitURL(archivedURL.Original)
	isYouTube := utils.IsYouTubeURL(archivedURL.Original)
	var gitRepoName string
	if isGit {
		gitRepoName = utils.ExtractRepoName(archivedURL.Original)
	}
	
	// Generate filename for downloads
	filename := utils.GenerateArchiveFilename(capture, archivedURL, targetItem.Extension)
	
	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":          capture.Timestamp.Format(time.RFC1123),
		"timestamp":     capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"archives":      capture.ArchiveItems,
		"current_item":  targetItem,
		"current_type":  urlType, // Use the URL type for display
		"short_id":      shortID,
		"host":          c.Request.Host,
		"original_url":  archivedURL.Original,
		"is_git":        isGit,
		"is_youtube":    isYouTube,
		"git_repo_name": gitRepoName,
		"download_filename": filename,
	})
}

func GetLogs(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	urlType := c.Param("type")
	
	// Convert URL type to internal type for database lookup
	internalType := urlTypeToInternalType(urlType)
	
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, internalType).
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": item.Logs, "status": item.Status})
}
