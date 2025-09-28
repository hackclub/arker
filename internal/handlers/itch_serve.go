package handlers

import (
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils/zipfs"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ServeItchHealth serves a simple health check for itch routes
func ServeItchHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "itch routes working",
		"time": fmt.Sprintf("%d", time.Now().Unix()),
	})
}

// ServeItchFile serves individual files from an itch archive
func ServeItchFile(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	filePath := c.Param("filepath")
	
	// Add timeout to prevent hanging
	c.Header("X-Debug-Route", "itch-file-serving")



	// URL decode and clean the file path
	decodedPath, err := url.QueryUnescape(filePath)
	if err != nil {
		// If URL decoding fails, use original path
		decodedPath = filePath
	}
	
	filePath = path.Clean(strings.TrimPrefix(decodedPath, "/"))
	if filePath == "." {
		filePath = ""
	}


	// Find the itch archive item
	var item models.ArchiveItem
	var capture models.Capture
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "itch").
		First(&item).Error; err != nil {

		c.Status(http.StatusNotFound)
		return
	}
	


	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}

	if err := db.Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Get a fresh reader for this request
	reader, err := storageInstance.Reader(item.StorageKey)
	if err != nil {

		c.Status(http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	// Read the entire archive into memory for this request
	// TODO: This is inefficient - should use seekable ZSTD for production
	archiveData, err := io.ReadAll(reader)
	if err != nil {
		c.Header("X-Debug-Error", "failed-to-read-archive")
		c.Status(http.StatusInternalServerError)
		return
	}
	
	// Add debug header with archive size
	c.Header("X-Debug-Archive-Size", fmt.Sprintf("%d", len(archiveData)))



	// Create a bytes reader for the archive
	archiveReader := &bytesReaderAt{data: archiveData}
	
	// Open ZIP archive
	archive, err := zipfs.OpenArchive(archiveReader, int64(len(archiveData)))
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// Find the requested file
	entry, found := archive.Lookup(filePath)
	if !found {
		c.Status(http.StatusNotFound)
		return
	}

	// Check if this is a range request
	rangeHeader := c.GetHeader("Range")
	
	// Get content type
	contentType := mime.TypeByExtension(path.Ext(filePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Set security headers
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Accept-Ranges", "bytes")

	// Generate ETag
	etag := fmt.Sprintf("\"%s-%d-%x\"", item.StorageKey, entry.UncompressedSize, entry.CRC32)
	c.Header("ETag", etag)

	// Check if-none-match
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}

	// Open the file
	fileReader, err := entry.Open(archive)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// Handle range requests
	if rangeHeader != "" && strings.HasPrefix(rangeHeader, "bytes=") {
		serveRangeRequest(c, fileReader, rangeHeader, int64(entry.UncompressedSize), contentType)
	} else {
		// Serve full file
		c.Header("Content-Type", contentType)
		c.Header("Content-Length", fmt.Sprintf("%d", entry.UncompressedSize))
		c.Status(http.StatusOK)
		io.Copy(c.Writer, fileReader)
	}
}

// serveRangeRequest handles HTTP range requests for individual files
func serveRangeRequest(c *gin.Context, reader io.Reader, rangeHeader string, fileSize int64, contentType string) {
	// Parse range header: "bytes=start-end"
	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		c.Status(http.StatusBadRequest)
		return
	}

	var start, end int64
	var err error

	if parts[0] == "" {
		// Suffix range: bytes=-500
		if parts[1] == "" {
			c.Status(http.StatusBadRequest)
			return
		}
		suffixLen, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffixLen <= 0 {
			c.Status(http.StatusBadRequest)
			return
		}
		start = fileSize - suffixLen
		if start < 0 {
			start = 0
		}
		end = fileSize - 1
	} else {
		// Regular range: bytes=0-499 or bytes=500-
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			c.Status(http.StatusBadRequest)
			return
		}
		
		if parts[1] == "" {
			// Open-ended range: bytes=500-
			end = fileSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				c.Status(http.StatusBadRequest)
				return
			}
		}
	}

	// Clamp to file boundaries
	if start >= fileSize {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", fileSize))
		c.Status(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= fileSize {
		end = fileSize - 1
	}

	contentLength := end - start + 1

	// Skip to start position
	if start > 0 {
		io.CopyN(io.Discard, reader, start)
	}

	// Set headers for partial content
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", fmt.Sprintf("%d", contentLength))
	c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	c.Status(http.StatusPartialContent)

	// Copy the requested range
	io.CopyN(c.Writer, reader, contentLength)
}

// bytesReaderAt implements io.ReaderAt for a byte slice
type bytesReaderAt struct {
	data []byte
}

func (r *bytesReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	
	n = copy(p, r.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	
	return n, err
}

// ServeItchGameList serves a JSON list of game files for API access
func ServeItchGameList(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")

	// Find the itch archive item
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "itch").
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Archive not found"})
		return
	}

	if item.Status != "completed" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Archive not ready"})
		return
	}

	// Read archive and get file list
	reader, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read archive"})
		return
	}
	defer reader.Close()

	archiveData, err := io.ReadAll(reader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read archive data"})
		return
	}

	archiveReader := &bytesReaderAt{data: archiveData}
	archive, err := zipfs.OpenArchive(archiveReader, int64(len(archiveData)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open archive"})
		return
	}

	// Build file list
	files := make([]gin.H, 0)
	for name, entry := range archive.Files() {
		files = append(files, gin.H{
			"name":             name,
			"size":             entry.UncompressedSize,
			"compressed_size":  entry.CompressedSize,
			"url":              fmt.Sprintf("/itch/%s/file/%s", shortID, url.PathEscape(name)),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"shortid": shortID,
		"files":   files,
	})
}
