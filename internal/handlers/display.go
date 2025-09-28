package handlers

import (
	"arker/internal/models"
	"arker/internal/utils"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
	"time"
)

// calculateQueuePosition returns the position of a pending job in the queue
func calculateQueuePosition(db *gorm.DB, item *models.ArchiveItem) int {
	if item.Status != "pending" {
		return 0
	}

	var count int64
	// Count pending items that were created before this item
	db.Model(&models.ArchiveItem{}).
		Where("status = 'pending' AND created_at < ?", item.CreatedAt).
		Count(&count)

	return int(count) + 1 // Add 1 because position is 1-based
}

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
	switch internalType {
	case "mhtml":
		return "Web"
	case "itch":
		return "Itch"
	default:
		return internalType
	}
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
	isVideo := utils.IsVideoURL(archivedURL.Original)
	isItch := utils.IsItchURL(archivedURL.Original)
	var defaultType string

	if isItch {
		// For itch.io URLs, prefer itch -> mhtml -> screenshot -> youtube -> git
		for _, preferredType := range []string{"itch", "mhtml", "screenshot", "youtube", "git"} {
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
	} else if isGit {
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
	} else if isVideo {
		// For video URLs (YouTube, Vimeo, etc.), prefer youtube -> mhtml -> screenshot -> git
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

	// Calculate queue position if item is pending
	queuePosition := calculateQueuePosition(db, targetItem)

	// Serve the default archive type view directly
	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":              capture.Timestamp.Format(time.RFC1123),
		"timestamp":         capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"archives":          capture.ArchiveItems,
		"current_item":      targetItem,
		"current_type":      internalTypeToURLType(defaultType), // Convert to URL type for display
		"short_id":          shortID,
		"host":              c.Request.Host,
		"original_url":      archivedURL.Original,
		"is_git":            isGit,
		"is_video":          isVideo,
		"git_repo_name":     gitRepoName,
		"download_filename": filename,
		"queue_position":    queuePosition,
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
	isVideo := utils.IsVideoURL(archivedURL.Original)
	var gitRepoName string
	if isGit {
		gitRepoName = utils.ExtractRepoName(archivedURL.Original)
	}

	// Generate filename for downloads
	filename := utils.GenerateArchiveFilename(capture, archivedURL, targetItem.Extension)

	// Calculate queue position if item is pending
	queuePosition := calculateQueuePosition(db, targetItem)

	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":              capture.Timestamp.Format(time.RFC1123),
		"timestamp":         capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"archives":          capture.ArchiveItems,
		"current_item":      targetItem,
		"current_type":      urlType, // Use the URL type for display
		"short_id":          shortID,
		"host":              c.Request.Host,
		"original_url":      archivedURL.Original,
		"is_git":            isGit,
		"is_video":          isVideo,
		"git_repo_name":     gitRepoName,
		"download_filename": filename,
		"queue_position":    queuePosition,
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
