package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

const maxVideoMetadataSize = 32 * 1024 * 1024

type videoManifestResponse struct {
	SchemaVersion             string          `json:"schema_version"`
	ShortID                   string          `json:"short_id"`
	CaptureStatus             string          `json:"capture_status"`
	MediaURL                  *string         `json:"media_url"`
	MetadataAvailable         bool            `json:"metadata_available"`
	Metadata                  json.RawMessage `json:"metadata"`
	RawMetadataURL            *string         `json:"raw_metadata_url"`
	Provenance                string          `json:"provenance,omitempty"`
	MetadataUnavailableReason string          `json:"metadata_unavailable_reason,omitempty"`
}

// ServeVideoManifest returns the stable normalized post metadata, capture
// status, and the existing playable media URL. It deliberately returns 200 for
// pending, failed, and legacy captures so API consumers can distinguish those
// states without scraping logs or the HTML viewer.
func ServeVideoManifest(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}

	item, ok := findVideoItem(c, db, shortID)
	if !ok {
		return
	}
	response := videoManifestResponse{
		SchemaVersion:     archivers.VideoMetadataSchemaVersion,
		ShortID:           shortID,
		CaptureStatus:     item.Status,
		MetadataAvailable: false,
		Metadata:          json.RawMessage("null"),
		Provenance:        item.Source,
	}
	if item.Status == "completed" && item.StorageKey != "" {
		mediaURL := fmt.Sprintf("/archive/%s/%s", shortID, utils.ArchiveTypeYtDlp)
		response.MediaURL = &mediaURL
	}

	if item.MetadataKey == "" {
		if item.Status == "completed" {
			response.MetadataUnavailableReason = "legacy_archive_without_structured_metadata"
		} else {
			response.MetadataUnavailableReason = "capture_not_completed"
		}
		c.JSON(http.StatusOK, response)
		return
	}

	metadata, err := readStoredJSON(store, item.MetadataKey, maxVideoMetadataSize)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "video metadata temporarily unavailable"})
		return
	}
	response.MetadataAvailable = true
	response.Metadata = metadata
	response.MetadataUnavailableReason = ""
	if item.RawMetadataKey != "" {
		rawURL := fmt.Sprintf("/video/%s/raw", shortID)
		response.RawMetadataURL = &rawURL
	}
	c.JSON(http.StatusOK, response)
}

// ServeVideoRawMetadata exposes the sanitized provider-native record. The
// archiver sanitizes before persistence, so this endpoint never needs access to
// cookies, credentials, authorization headers, or private proxy details.
func ServeVideoRawMetadata(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}
	item, ok := findVideoItem(c, db, shortID)
	if !ok {
		return
	}
	if item.Status != "completed" || item.RawMetadataKey == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "raw video metadata not available"})
		return
	}

	reader, err := store.Reader(item.RawMetadataKey)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "raw video metadata temporarily unavailable"})
		return
	}
	defer reader.Close()
	c.Header("Content-Type", "application/json")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("ETag", fmt.Sprintf("\"%s\"", item.RawMetadataKey))
	if size, err := store.Size(item.RawMetadataKey); err == nil {
		c.Header("Content-Length", fmt.Sprintf("%d", size))
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, reader)
}

func findVideoItem(c *gin.Context, db *gorm.DB, shortID string) (models.ArchiveItem, bool) {
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type IN ?", shortID, utils.ArchiveTypeMatchValues(utils.ArchiveTypeYtDlp)).
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video archive not found"})
		return models.ArchiveItem{}, false
	}
	return item, true
}

func readStoredJSON(store storage.Storage, key string, limit int64) (json.RawMessage, error) {
	reader, err := store.Reader(key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("metadata exceeds %d bytes", limit)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("stored metadata is not valid JSON")
	}
	return raw, nil
}
