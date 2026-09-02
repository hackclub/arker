package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

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

	item, err := lookupVideoItem(db, shortID)
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "video archive not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
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
	metadata = normalizeVideoManifestMetadata(metadata)
	response.MetadataAvailable = true
	response.Metadata = metadata
	response.MetadataUnavailableReason = ""
	if item.RawMetadataKey != "" {
		rawURL := fmt.Sprintf("/video/%s/raw", shortID)
		response.RawMetadataURL = &rawURL
	}
	c.JSON(http.StatusOK, response)
}

// normalizeVideoManifestMetadata makes the consumer-facing required keys
// unconditional while retaining every additive field in the stored sidecar.
// A provider that genuinely did not expose a value yields null, never a
// disappearing key that forces per-platform schema branches downstream.
func normalizeVideoManifestMetadata(raw json.RawMessage) json.RawMessage {
	var metadata map[string]interface{}
	if json.Unmarshal(raw, &metadata) != nil {
		return raw
	}
	for _, key := range []string{"duration_seconds", "title", "channel", "publication_timestamp"} {
		if _, ok := metadata[key]; !ok {
			metadata[key] = nil
		}
	}
	if metadata["title"] == nil || metadata["title"] == "" {
		if description, ok := metadata["description"].(string); ok && description != "" {
			metadata["title"] = description
		}
	}
	if metadata["channel"] == nil || metadata["channel"] == "" {
		for _, key := range []string{"author", "uploader"} {
			if value, ok := metadata[key].(string); ok && value != "" {
				metadata["channel"] = value
				break
			}
		}
	}
	if metadata["channel"] == nil || metadata["channel"] == "" {
		for _, key := range []string{"author_id", "uploader_id"} {
			if value, ok := metadata[key].(string); ok && strings.HasPrefix(value, "@") && len(value) > 1 {
				metadata["channel"] = strings.TrimPrefix(value, "@")
				break
			}
		}
	}
	if metadata["channel"] == nil || metadata["channel"] == "" {
		if sourceURL, ok := metadata["source_url"].(string); ok {
			if handle := socialProfileHandle(sourceURL); handle != "" {
				metadata["channel"] = handle
			}
		}
	}
	media, _ := metadata["media"].(map[string]interface{})
	if media == nil {
		media = map[string]interface{}{}
		metadata["media"] = media
	}
	for _, key := range []string{"width", "height"} {
		if _, ok := media[key]; !ok {
			media[key] = nil
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return encoded
}

// socialProfileHandle returns attribution only when the provider-authored
// source URL itself identifies a social profile. Post IDs and reserved route
// names are deliberately not treated as channel names.
func socialProfileHandle(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	switch host {
	case "tiktok.com":
		if strings.HasPrefix(parts[0], "@") && len(parts[0]) > 1 {
			return strings.TrimPrefix(parts[0], "@")
		}
	case "instagram.com":
		if len(parts) == 1 {
			switch strings.ToLower(parts[0]) {
			case "p", "reel", "reels", "stories", "explore", "accounts":
				return ""
			default:
				return strings.TrimPrefix(parts[0], "@")
			}
		}
	}
	return ""
}

// ServeVideoRawMetadata exposes the sanitized provider-native record. The
// archiver sanitizes before persistence, so this endpoint never needs access to
// cookies, credentials, authorization headers, or private proxy details.
func ServeVideoRawMetadata(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	item, err := lookupVideoItem(db, shortID)
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "raw video metadata not available"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
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

func lookupVideoItem(db *gorm.DB, shortID string) (models.ArchiveItem, error) {
	capture, err := resolveCaptureForMachineEndpoint(db, shortID)
	if err != nil {
		return models.ArchiveItem{}, err
	}
	var item models.ArchiveItem
	if err := db.Where("capture_id = ? AND type IN ?", capture.ID, utils.ArchiveTypeMatchValues(utils.ArchiveTypeYtDlp)).
		First(&item).Error; err != nil {
		return models.ArchiveItem{}, err
	}
	return item, nil
}

func findVideoItem(c *gin.Context, db *gorm.DB, shortID string) (models.ArchiveItem, bool) {
	item, err := lookupVideoItem(db, shortID)
	if err != nil {
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
