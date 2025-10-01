package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ServeArchive(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	typ := c.Param("type")
	var item models.ArchiveItem
	var capture models.Capture
	var archivedURL models.ArchivedURL
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, typ).
		First(&item).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := db.Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := db.Where("id = ?", capture.ArchivedURLID).First(&archivedURL).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}
	r, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()
	var ct string
	attach := false
	switch typ {
	case "mhtml":
		ct = "multipart/related" // Original MHTML content type for downloads
		attach = true
	case "screenshot":
		ct = "image/webp"
	case "youtube":
		ct = "video/mp4"
	case "git":
		ct = "application/x-tar"
		attach = true
	case "itch":
		ct = "application/zip"
		attach = true
	default:
		ct = "application/octet-stream"
		attach = true
	}
	c.Header("Content-Type", ct)

	// Explicitly clear any automatic content-encoding detection
	c.Header("Content-Encoding", "")

	// Add ETag for conditional requests
	c.Header("ETag", fmt.Sprintf("\"%s-%d\"", item.StorageKey, item.FileSize))

	if attach {
		filename := utils.GenerateArchiveFilename(capture, archivedURL, item.Extension)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	}

	// Set content length from storage
	if fileSize, err := storageInstance.Size(item.StorageKey); err == nil {
		c.Header("Content-Length", fmt.Sprintf("%d", fileSize))
	}

	// Stream the file directly
	_, err = io.Copy(c.Writer, r)
	if err != nil {
		log.Printf("Error streaming file %s: %v", item.StorageKey, err)
		// Note: We can't change the response status here since headers are already sent
		// The connection will be closed which the client will detect
	}
}

func ServeMHTMLAsHTML(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "mhtml").
		First(&item).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}

	r, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		log.Printf("Failed to open storage for %s: %v", shortID, err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()

	c.Header("Content-Type", "text/html")

	log.Printf("Converting MHTML to HTML for %s", shortID)

	// Use the MHTML converter with streaming (decompression now handled by storage)
	converter := utils.NewStreamingConverter()
	if err := converter.ConvertMHTMLToHTML(r, c.Writer); err != nil {
		log.Printf("MHTML conversion error for %s: %v", shortID, err)
		c.String(http.StatusInternalServerError, "MHTML conversion failed: %v", err)
		return
	}
}
