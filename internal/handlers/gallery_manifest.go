package handlers

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// maxGalleryManifestMetadataSize caps the normalized metadata block copied out
// of a bundle. It is a post record, not media, so this is already generous.
const maxGalleryManifestMetadataSize = 4 * 1024 * 1024

// galleryManifestResponse is the public gallery contract, deliberately shaped
// like videoManifestResponse: same envelope fields, same meanings, so a
// consumer handles a carousel and a reel the same way.
//
// The difference is that a gallery post is many assets, so the single
// media_url becomes an ordered media list. Every URL is absolute and complete:
// a consumer must never have to build a path out of the capture tool's name.
type galleryManifestResponse struct {
	SchemaVersion string                 `json:"schema_version"`
	ShortID       string                 `json:"short_id"`
	CaptureStatus string                 `json:"capture_status"`
	MediaCount    int                    `json:"media_count"`
	Media         []galleryManifestMedia `json:"media"`
	// ThumbnailURL is the post's archived preview image, carrying exactly the
	// meaning it carries in the video manifest so a consumer handles a carousel
	// and a reel the same way: an Arker-stored image, or null when the capture
	// stored none.
	//
	// A post's cover is its first card, which is what the archiver derives the
	// stored thumbnail from. A post whose cards are all video falls back to the
	// archived page screenshot rather than a frame Arker never captured.
	ThumbnailURL              *string         `json:"thumbnail_url"`
	ArchiveURL                *string         `json:"archive_url"`
	MetadataAvailable         bool            `json:"metadata_available"`
	Metadata                  json.RawMessage `json:"metadata"`
	RawMetadataURL            *string         `json:"raw_metadata_url"`
	Provenance                string          `json:"provenance,omitempty"`
	MetadataUnavailableReason string          `json:"metadata_unavailable_reason,omitempty"`
}

// galleryManifestMedia is one card of the post.
//
// Index is the card's position in the post as published, counting from 0 in
// swipe order. It is reported rather than left implicit because attention
// decays by position, so consumers weight by it, and neither JSON array order
// alone nor a provider filename is a contract they should have to trust.
type galleryManifestMedia struct {
	Index       int    `json:"index"`
	MediaURL    string `json:"media_url"`
	Filename    string `json:"filename"`
	Type        string `json:"type"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Width       *int64 `json:"width,omitempty"`
	Height      *int64 `json:"height,omitempty"`
	AltText     string `json:"alt_text,omitempty"`
}

// ServeGalleryManifest returns a gallery capture's status, normalized post
// metadata, and a directly fetchable URL per card, in swipe order.
//
// Like the video manifest it returns 200 for pending, failed and legacy
// captures so a consumer can distinguish those states without scraping logs,
// and it is keyless for the same reason: it describes exactly the archive the
// public viewer already renders.
func ServeGalleryManifest(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}

	item, ok := findGalleryItem(c, db, shortID)
	if !ok {
		return
	}

	response := galleryManifestResponse{
		SchemaVersion: archivers.GalleryMetadataSchemaVersion,
		ShortID:       shortID,
		CaptureStatus: item.Status,
		// Never null: consumers iterate this unconditionally.
		Media:      []galleryManifestMedia{},
		Metadata:   json.RawMessage("null"),
		Provenance: item.Source,
	}

	// Resolved before the not-completed return below so an unfinished capture
	// that already has, say, its screenshot still reports that preview.
	if captureHasStoredThumbnail(db, item) {
		thumbnailURL := fullPath(c, fmt.Sprintf("thumb/%s", shortID))
		response.ThumbnailURL = &thumbnailURL
	}

	if item.Status != "completed" || item.StorageKey == "" {
		response.MetadataUnavailableReason = "capture_not_completed"
		c.JSON(http.StatusOK, response)
		return
	}

	zipReader, cleanup, err := openGalleryZipItem(store, &item)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "gallery archive temporarily unavailable"})
		return
	}
	defer cleanup()

	archiveURL := fullPath(c, fmt.Sprintf("archive/%s/%s", shortID, utils.ArchiveTypeGalleryDl))
	response.ArchiveURL = &archiveURL

	var metadata archivers.GalleryMetadata
	rawMetadataAvailable := false
	mediaEntries := make([]*zip.File, 0, len(zipReader.File))
	for _, file := range zipReader.File {
		switch {
		case file.Name == galleryMetadataFilename:
			if raw, err := readGalleryZipEntry(file, maxGalleryManifestMetadataSize); err == nil && json.Valid(raw) {
				response.MetadataAvailable = true
				response.Metadata = raw
				_ = json.Unmarshal(raw, &metadata)
			}
		case strings.HasSuffix(strings.ToLower(file.Name), ".json"):
			// A provider sidecar. Its sanitized contents are served by
			// /gallery/:shortid/raw, never inlined here.
			rawMetadataAvailable = true
		default:
			mediaEntries = append(mediaEntries, file)
		}
	}
	if !response.MetadataAvailable {
		response.MetadataUnavailableReason = "legacy_archive_without_structured_metadata"
	}
	if rawMetadataAvailable {
		rawURL := fullPath(c, fmt.Sprintf("gallery/%s/raw", shortID))
		response.RawMetadataURL = &rawURL
	}

	sortGalleryMediaEntries(mediaEntries)

	details := make(map[string]archivers.GalleryFile, len(metadata.Files))
	for _, file := range metadata.Files {
		details[file.Name] = file
	}
	for _, file := range mediaEntries {
		contentType := galleryZipFileContentType(file)
		card := galleryManifestMedia{
			Index:       len(response.Media),
			MediaURL:    fullPath(c, fmt.Sprintf("gallery/%s/file/%s", shortID, url.PathEscape(file.Name))),
			Filename:    file.Name,
			Type:        galleryMediaKind(contentType),
			ContentType: contentType,
			SizeBytes:   int64(file.UncompressedSize64),
		}
		if stored, ok := details[file.Name]; ok {
			if stored.Width > 0 {
				width := int64(stored.Width)
				card.Width = &width
			}
			if stored.Height > 0 {
				height := int64(stored.Height)
				card.Height = &height
			}
			card.AltText = stored.AltText
		}
		response.Media = append(response.Media, card)
	}
	response.MediaCount = len(response.Media)

	c.JSON(http.StatusOK, response)
}

// findGalleryItem resolves a capture's gallery item, answering 404 in the same
// shape as the video manifest when the capture has no gallery media at all.
func findGalleryItem(c *gin.Context, db *gorm.DB, shortID string) (models.ArchiveItem, bool) {
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type IN ?", shortID, utils.ArchiveTypeMatchValues(utils.ArchiveTypeGalleryDl)).
		First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "gallery archive not found"})
		return models.ArchiveItem{}, false
	}
	return item, true
}

// sortGalleryMediaEntries puts the cards back in swipe order.
//
// The archiver names files by slide number (001.jpg), so ordering is recorded
// in the names rather than in ZIP order. Comparing the leading number rather
// than the string keeps a legacy or provider-supplied bundle that used
// unpadded names from sorting slide 10 before slide 2.
func sortGalleryMediaEntries(entries []*zip.File) {
	sort.SliceStable(entries, func(i, j int) bool {
		left, leftOK := gallerySlideNumber(entries[i].Name)
		right, rightOK := gallerySlideNumber(entries[j].Name)
		if leftOK && rightOK && left != right {
			return left < right
		}
		if leftOK != rightOK {
			// Numbered slides are the post; anything else trails it.
			return leftOK
		}
		return entries[i].Name < entries[j].Name
	})
}

// gallerySlideNumber reads the leading slide number off a stored filename,
// reporting false for any name that is not numbered.
func gallerySlideNumber(name string) (int, bool) {
	base := name
	if dot := strings.Index(base, "."); dot >= 0 {
		base = base[:dot]
	}
	if base == "" {
		return 0, false
	}
	number := 0
	for _, r := range base {
		if r < '0' || r > '9' {
			return 0, false
		}
		number = number*10 + int(r-'0')
		if number > maxGallerySlideNumber {
			return 0, false
		}
	}
	return number, true
}

// maxGallerySlideNumber bounds slide-number parsing so a pathological filename
// cannot overflow the accumulator.
const maxGallerySlideNumber = 100000

// galleryMediaKind classifies a card for consumers that only want stills or
// only want video, without making them parse MIME types.
func galleryMediaKind(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio"
	default:
		return "other"
	}
}

func readGalleryZipEntry(file *zip.File, limit int64) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("gallery metadata exceeds %d bytes", limit)
	}
	return raw, nil
}
