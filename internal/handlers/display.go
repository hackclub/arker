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

// URL type mapping: user-facing URLs use "web" instead of "mhtml".
//
// This also resolves retired type names, so permalinks handed out before the
// yt-dlp rename (/{shortid}/youtube) keep working forever.
func urlTypeToInternalType(urlType string) string {
	if urlType == "web" {
		return utils.ArchiveTypeMHTML
	}
	return utils.NormalizeArchiveType(urlType)
}

// internalTypeToURLType maps a stored type to the segment used in links. It
// canonicalizes, so a row still holding a retired type name links to (and
// highlights as) its current name rather than producing a tab that 404s.
func internalTypeToURLType(internalType string) string {
	canonical := utils.NormalizeArchiveType(internalType)
	if canonical == utils.ArchiveTypeMHTML {
		return "web"
	}
	return canonical
}

func getDisplayName(internalType string) string {
	switch utils.NormalizeArchiveType(internalType) {
	case utils.ArchiveTypeMHTML:
		return "Web"
	case utils.ArchiveTypeItch:
		return "Itch"
	case utils.ArchiveTypeYtDlp:
		return "Video"
	case utils.ArchiveTypeGalleryDl:
		return "Media"
	default:
		return internalType
	}
}

// defaultTypePreference returns the archive types to land on, best first, for
// the kind of page this URL is.
func defaultTypePreference(originalURL string) []string {
	switch {
	case utils.IsItchURL(originalURL):
		return []string{utils.ArchiveTypeItch, utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot, utils.ArchiveTypeYtDlp, utils.ArchiveTypeGit}
	case utils.IsGitURL(originalURL):
		return []string{utils.ArchiveTypeGit, utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot, utils.ArchiveTypeYtDlp}
	case utils.IsGalleryDLURL(originalURL):
		return []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot, utils.ArchiveTypeYtDlp, utils.ArchiveTypeGit}
	case utils.IsVideoURL(originalURL):
		return []string{utils.ArchiveTypeYtDlp, utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot, utils.ArchiveTypeGit}
	default:
		return []string{utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot, utils.ArchiveTypeGit, utils.ArchiveTypeYtDlp}
	}
}

// archiveTab is one rendered tab in the viewer.
type archiveTab struct {
	URLType     string
	DisplayName string
	Status      string
	IsActive    bool
}

// buildTabs orders a capture's archive items the way the viewer should show
// them: the types this kind of URL cares about most first, then anything else
// in creation order. Building this server-side keeps the template to a single
// loop instead of one near-identical copy per URL kind.
func buildTabs(items []models.ArchiveItem, preference []string, currentURLType string) []archiveTab {
	tabs := make([]archiveTab, 0, len(items))
	used := make(map[string]bool, len(items))

	appendTab := func(item models.ArchiveItem) {
		urlType := internalTypeToURLType(item.Type)
		tabs = append(tabs, archiveTab{
			URLType:     urlType,
			DisplayName: getDisplayName(item.Type),
			Status:      item.Status,
			IsActive:    urlType == currentURLType,
		})
	}

	for _, preferredType := range preference {
		for _, item := range items {
			canonical := utils.NormalizeArchiveType(item.Type)
			if canonical == preferredType && !used[canonical] {
				used[canonical] = true
				appendTab(item)
			}
		}
	}
	for _, item := range items {
		canonical := utils.NormalizeArchiveType(item.Type)
		if !used[canonical] {
			used[canonical] = true
			appendTab(item)
		}
	}
	return tabs
}

// selectDefaultType picks which tab to open.
//
// Preference order decides, except that a failed archive is skipped while any
// non-failed one exists: landing a visitor on a red "Archive Failed" page while
// a perfectly good screenshot sits one tab over is the worst of the available
// options. Pending and processing archives are deliberately still eligible — a
// freshly queued capture should open on the tab that is working, with its live
// log and auto-reload, not on whichever fast archive happened to finish first.
func selectDefaultType(items []models.ArchiveItem, preference []string) string {
	byType := make(map[string]string, len(items))
	for _, item := range items {
		byType[utils.NormalizeArchiveType(item.Type)] = item.Status
	}

	for _, preferredType := range preference {
		if status, ok := byType[preferredType]; ok && status != "failed" {
			return preferredType
		}
	}
	for _, preferredType := range preference {
		if _, ok := byType[preferredType]; ok {
			return preferredType
		}
	}
	for _, item := range items {
		if item.Status != "failed" {
			return utils.NormalizeArchiveType(item.Type)
		}
	}
	if len(items) > 0 {
		return utils.NormalizeArchiveType(items[0].Type)
	}
	return ""
}

// DisplayDefault serves the default archive type view directly (no redirect)
func DisplayDefault(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}
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

	preference := defaultTypePreference(archivedURL.Original)
	defaultType := selectDefaultType(capture.ArchiveItems, preference)

	if defaultType == "" {
		c.Status(http.StatusNotFound)
		return
	}

	// Find the specific archive item
	var targetItem *models.ArchiveItem
	for i := range capture.ArchiveItems {
		if utils.ArchiveTypesEqual(capture.ArchiveItems[i].Type, defaultType) {
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
	if logs, err := utils.ArchiveItemLogString(db, targetItem.ID, targetItem.Logs); err == nil {
		targetItem.Logs = logs
	}
	thumbnailURL, thumbnailWidth, thumbnailHeight := displayThumbnail(c, capture.ArchiveItems, preference, shortID)

	// Serve the default archive type view directly
	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":              capture.Timestamp.Format(time.RFC1123),
		"timestamp":         capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"tabs":              buildTabs(capture.ArchiveItems, preference, internalTypeToURLType(defaultType)),
		"current_item":      targetItem,
		"current_type":      internalTypeToURLType(defaultType), // Convert to URL type for display
		"short_id":          shortID,
		"host":              c.Request.Host,
		"original_url":      archivedURL.Original,
		"git_repo_name":     gitRepoName,
		"download_filename": filename,
		"queue_position":    queuePosition,
		"thumbnail_url":     thumbnailURL,
		"thumbnail_width":   thumbnailWidth,
		"thumbnail_height":  thumbnailHeight,
		"archive_url":       utils.BuildFullURL(c, shortID),
	})
}

// DisplayType shows a specific archive type page
func DisplayType(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	urlType := c.Param("type")
	if redirectIfAlias(c, db, shortID) {
		return
	}

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

	// Find the specific archive item using internal type. Compares canonically
	// so a row still holding a retired type name stays reachable.
	var targetItem *models.ArchiveItem
	for i := range capture.ArchiveItems {
		if utils.ArchiveTypesEqual(capture.ArchiveItems[i].Type, internalType) {
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
	if utils.IsGitURL(archivedURL.Original) {
		gitRepoName = utils.ExtractRepoName(archivedURL.Original)
	}

	// Generate filename for downloads
	filename := utils.GenerateArchiveFilename(capture, archivedURL, targetItem.Extension)

	// Calculate queue position if item is pending
	queuePosition := calculateQueuePosition(db, targetItem)
	if logs, err := utils.ArchiveItemLogString(db, targetItem.ID, targetItem.Logs); err == nil {
		targetItem.Logs = logs
	}
	preference := defaultTypePreference(archivedURL.Original)
	thumbnailURL, thumbnailWidth, thumbnailHeight := displayThumbnail(c, capture.ArchiveItems, preference, shortID)

	c.HTML(http.StatusOK, "display_type.html", gin.H{
		"date":         capture.Timestamp.Format(time.RFC1123),
		"timestamp":    capture.Timestamp.Format(time.RFC3339), // For JavaScript parsing
		"tabs":         buildTabs(capture.ArchiveItems, preference, internalTypeToURLType(internalType)),
		"current_item": targetItem,
		// Canonicalize rather than echoing urlType: a legacy /{id}/youtube
		// permalink must still match the tab links, which are canonical.
		"current_type":      internalTypeToURLType(internalType),
		"short_id":          shortID,
		"host":              c.Request.Host,
		"original_url":      archivedURL.Original,
		"git_repo_name":     gitRepoName,
		"download_filename": filename,
		"queue_position":    queuePosition,
		"thumbnail_url":     thumbnailURL,
		"thumbnail_width":   thumbnailWidth,
		"thumbnail_height":  thumbnailHeight,
		"archive_url":       utils.BuildFullURL(c, shortID),
	})
}

func displayThumbnail(c *gin.Context, items []models.ArchiveItem, preference []string, shortID string) (string, int, int) {
	ready, _ := selectThumbnailItem(items, preference)
	if ready == nil {
		return ThumbnailURL(c, shortID), 0, 0
	}
	return ThumbnailURL(c, shortID, ready.ThumbnailKey), ready.ThumbnailWidth, ready.ThumbnailHeight
}

func GetLogs(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	urlType := c.Param("type")
	if redirectIfAlias(c, db, shortID) {
		return
	}

	// Convert URL type to internal type for database lookup
	internalType := urlTypeToInternalType(urlType)

	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type IN ?", shortID, utils.ArchiveTypeMatchValues(internalType)).
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	logs, err := utils.ArchiveItemLogString(db, item.ID, item.Logs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get logs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs, "status": item.Status, "retry_count": item.RetryCount})
}
