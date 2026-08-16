package handlers

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// galleryVideoProjection is the video view of a gallery capture that proved
// to be one complete video. Instagram is the motivating case: the same Reel
// is valid at both /p/{shortcode} and /reel/{shortcode}, but routing has to
// choose an extractor before either one reveals the post's media type.
type galleryVideoProjection struct {
	Item       models.ArchiveItem
	Entry      *zip.File
	Metadata   archivers.VideoMetadata
	HasRawData bool
}

// projectGalleryVideo returns a projection only when the stored artifact
// proves the post is exactly one complete video. Photos, mixed carousels,
// partial downloads, and legacy bundles without a completeness verdict stay
// galleries; silently collapsing any of those would lose part of the post.
func projectGalleryVideo(store storage.Storage, db *gorm.DB, shortID string) (*galleryVideoProjection, func(), error) {
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type IN ?", shortID, utils.ArchiveTypeMatchValues(utils.ArchiveTypeGalleryDl)).
		First(&item).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, func() {}, nil
		}
		return nil, func() {}, err
	}
	if item.Status != "completed" || item.StorageKey == "" {
		return nil, func() {}, nil
	}

	zr, cleanup, err := openGalleryZipItem(store, &item)
	if err != nil {
		return nil, func() {}, err
	}

	var gallery archivers.GalleryMetadata
	metadataFound := false
	hasRaw := false
	media := make([]*zip.File, 0, 2)
	for _, file := range zr.File {
		switch {
		case file.Name == galleryMetadataFilename:
			raw, readErr := readGalleryZipEntry(file, maxGalleryManifestMetadataSize)
			if readErr == nil && json.Unmarshal(raw, &gallery) == nil {
				metadataFound = true
			}
		case strings.HasSuffix(strings.ToLower(file.Name), ".json"):
			hasRaw = true
		default:
			media = append(media, file)
		}
	}
	if !metadataFound || len(media) != 1 || gallery.FileCount != 1 || gallery.Completeness == nil ||
		archivers.NormalizeCompletenessState(gallery.Completeness.State) != archivers.CompletenessComplete ||
		gallery.Completeness.Stored != 1 ||
		(gallery.Completeness.Expected != nil && *gallery.Completeness.Expected != 1) {
		cleanup()
		return nil, func() {}, nil
	}

	entry := media[0]
	contentType := galleryZipFileContentType(entry)
	if !strings.HasPrefix(contentType, "video/") {
		cleanup()
		return nil, func() {}, nil
	}

	var details archivers.GalleryFile
	for _, file := range gallery.Files {
		if file.Name == entry.Name {
			details = file
			break
		}
	}
	var width, height *int64
	if details.Width > 0 {
		value := int64(details.Width)
		width = &value
	}
	if details.Height > 0 {
		value := int64(details.Height)
		height = &value
	}
	provider := "gallery-dl"
	provenance := item.Source
	if provenance == "" {
		provenance = models.ArchiveSourceNative
	}
	if item.Source == models.ArchiveSourceBrightData {
		provider = "brightdata"
	}
	video := archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            gallery.SourceURL,
		Platform:             strings.ToLower(gallery.Extractor),
		Extractor:            gallery.Extractor,
		PostID:               gallery.PostID,
		CanonicalURL:         gallery.PostURL,
		Title:                gallery.Title,
		Description:          gallery.Description,
		Author:               gallery.Author,
		PublicationTimestamp: gallery.Date,
		Engagement:           archivers.VideoEngagement{Likes: gallery.Likes},
		Tags:                 gallery.Tags,
		Media: archivers.VideoMedia{
			Extension:   filepath.Ext(entry.Name),
			ContentType: contentType,
			SizeBytes:   int64(entry.UncompressedSize64),
			Width:       width,
			Height:      height,
		},
		ArchivedAt: gallery.ArchivedAt,
		Provenance: provenance,
		Provider:   provider,
	}
	if video.SourceURL == "" {
		video.SourceURL = gallery.PostURL
	}
	if video.CanonicalURL == "" {
		video.CanonicalURL = video.SourceURL
	}
	if video.Media.Extension == "" {
		video.Media.Extension = ".mp4"
	}

	return &galleryVideoProjection{Item: item, Entry: entry, Metadata: video, HasRawData: hasRaw}, cleanup, nil
}

func marshalProjectedVideoMetadata(projection *galleryVideoProjection) (json.RawMessage, error) {
	if projection == nil {
		return nil, fmt.Errorf("gallery video projection is nil")
	}
	raw, err := archivers.MarshalVideoMetadata(&projection.Metadata)
	return json.RawMessage(raw), err
}
