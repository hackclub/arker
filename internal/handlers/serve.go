package handlers

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

func ServeArchive(c *gin.Context, storage storage.Storage, db *gorm.DB) {
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
	r, err := storage.Reader(item.StorageKey)
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
		ct = "application/zstd"
		attach = true
	default:
		ct = "application/octet-stream"
		attach = true
	}
	c.Header("Content-Type", ct)
	
	// Set content length for better streaming performance
	if item.FileSize > 0 {
		c.Header("Content-Length", fmt.Sprintf("%d", item.FileSize))
	}
	
	// Add caching headers for better performance
	c.Header("Cache-Control", "public, max-age=86400") // Cache for 24 hours
	c.Header("ETag", fmt.Sprintf("\"%s-%d\"", item.StorageKey, item.FileSize))
	
	if attach {
		filename := utils.GenerateArchiveFilename(capture, archivedURL, item.Extension)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	}
	
	// Stream the file
	io.Copy(c.Writer, r)
}

func ServeMHTMLAsHTML(c *gin.Context, storage storage.Storage, db *gorm.DB) {
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
	
	r, err := storage.Reader(item.StorageKey)
	if err != nil {
		log.Printf("Failed to open storage for %s: %v", shortID, err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()
	
	c.Header("Content-Type", "text/html")
	
	log.Printf("Converting MHTML to HTML for %s", shortID)
	
	// Use the MHTML converter with streaming (decompression now handled by storage)
	converter := &utils.MHTMLConverter{}
	if err := converter.Convert(r, c.Writer); err != nil {
		log.Printf("MHTML conversion error for %s: %v", shortID, err)
		c.String(http.StatusInternalServerError, "MHTML conversion failed: %v", err)
		return
	}
}
