package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

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

	serveArchiveContent(c, storageInstance, item, capture, archivedURL)
}

func serveArchiveContent(c *gin.Context, storageInstance storage.Storage, item models.ArchiveItem, capture models.Capture, archivedURL models.ArchivedURL) {
	ct, attach := contentTypeForArchive(item.Type, item.Extension)
	filename := utils.GenerateArchiveFilename(capture, archivedURL, item.Extension)
	contentDisposition := ""
	if attach {
		contentDisposition = fmt.Sprintf("attachment; filename=\"%s\"", filename)
	}

	if directStorage, ok := storageInstance.(storage.DirectURLStorage); ok {
		directURL, err := directStorage.DirectURL(c.Request.Context(), item.StorageKey, storage.DirectURLOptions{
			Method:             c.Request.Method,
			ContentType:        ct,
			ContentDisposition: contentDisposition,
		})
		if err == nil && directURL != "" {
			c.Header("Location", directURL)
			c.AbortWithStatus(http.StatusTemporaryRedirect)
			return
		}
		if err != nil {
			log.Printf("Failed to generate direct archive URL for short_id=%s storage_key=%s: %v", capture.ShortID, item.StorageKey, err)
		} else {
			log.Printf("Direct archive URL was empty for short_id=%s storage_key=%s", capture.ShortID, item.StorageKey)
		}
	}

	c.Header("Content-Type", ct)

	// Explicitly clear any automatic content-encoding detection
	c.Header("Content-Encoding", "")

	// Add ETag for conditional requests
	c.Header("ETag", fmt.Sprintf("\"%s-%d\"", item.StorageKey, item.FileSize))

	if contentDisposition != "" {
		c.Header("Content-Disposition", contentDisposition)
	}

	if seekableStorage, ok := storageInstance.(storage.SeekableStorage); ok {
		r, err := seekableStorage.SeekableReader(item.StorageKey)
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		defer r.Close()

		http.ServeContent(c.Writer, c.Request, filename, time.Time{}, r)
		return
	}

	r, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()

	// Set content length from storage
	if fileSize, err := storageInstance.Size(item.StorageKey); err == nil {
		c.Header("Content-Length", fmt.Sprintf("%d", fileSize))
	}

	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}

	// Stream the file directly
	_, err = io.Copy(c.Writer, r)
	if err != nil {
		log.Printf("Error streaming file %s: %v", item.StorageKey, err)
		// Note: We can't change the response status here since headers are already sent
		// The connection will be closed which the client will detect
	}
}

func contentTypeForArchive(typ, extension string) (string, bool) {
	switch typ {
	case "mhtml":
		return "multipart/related", true // Original MHTML content type for downloads
	case "screenshot":
		return "image/webp", false
	case "youtube":
		switch strings.ToLower(extension) {
		case ".webm":
			return "video/webm", false
		default:
			return "video/mp4", false
		}
	case "git":
		return "application/x-tar", true
	case "itch":
		return "application/zip", true
	default:
		return "application/octet-stream", true
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
