package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"fmt"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"io"
	"log"
	"net/http"
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
		ct = "application/zstd"
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

	// Try to get uncompressed size efficiently from zstd storage (skip for MHTML since it's served differently)
	if typ != "mhtml" {
		if zstdStorage, ok := storageInstance.(*storage.ZSTDStorage); ok {
			if uncompressedSize, err := zstdStorage.UncompressedSize(item.StorageKey); err == nil {
				// Validate file integrity by trying to read first few bytes
				testReader, testErr := storageInstance.Reader(item.StorageKey)
				if testErr == nil {
					testBuf := make([]byte, 100)
					_, testReadErr := testReader.Read(testBuf)
					testReader.Close()

					if testReadErr != nil && testReadErr != io.EOF {
						log.Printf("File integrity check failed for %s: %v", item.StorageKey, testReadErr)
						c.Status(http.StatusInternalServerError)
						return
					}
				}

				c.Header("Content-Length", fmt.Sprintf("%d", uncompressedSize))
				log.Printf("Set Content-Length to %d for %s", uncompressedSize, item.StorageKey)
			} else {
				log.Printf("Failed to get uncompressed size for %s: %v", item.StorageKey, err)
			}
		} else {
			log.Printf("Storage is not ZSTDStorage type for %s", item.StorageKey)
		}
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
