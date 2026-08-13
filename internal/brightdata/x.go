package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// archiveX is the X/Twitter fallback entry point.
//
// X serves nothing to logged-out clients, so without a cookie jar the native
// gallery-dl run cannot succeed at all — which is why an X item is only created
// when this fallback is configured (utils.ShouldCreateGalleryDLItem). The Posts
// dataset returns the tweet plus its media URLs, and those download directly
// from Arker's own connection: pbs.twimg.com images and video.twimg.com MP4s
// are public and not IP-locked (verified 206 + real MP4 bytes).
//
// X routes to gallery-dl only, so every rescue produces a gallery bundle; a
// single-video tweet also gets the normalized video metadata sidecar, which is
// where views/likes/replies/reposts and the publication timestamp live.
func (c *Client) archiveX(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no X fallback for archive type %s", itemType)
	}

	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetXPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := xMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no media (a text-only post has nothing to archive)", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	logXMetadata(logWriter, record)

	meta := xGalleryMetadata(record, targetURL)
	result, completeness, totalBytes, err := c.buildGalleryArchive(ctx, entries, meta, record, c.directFetch, logWriter)
	if err != nil {
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	if videoURL := singleXVideo(entries); videoURL != "" {
		size := storedFileSize(meta, func(f archivers.GalleryFile) bool { return f.IsVideo })
		metadata, rawMetadata, buildErr := buildBrightDataXVideoArtifacts(record, targetURL, videoURL, size, time.Now())
		if buildErr != nil {
			closeResultData(result)
			usage.Detail = truncate("metadata build failed: "+buildErr.Error(), 500)
			c.recordUsage(db, usage)
			return archivers.Result{}, fmt.Errorf("failed to build Bright Data video metadata: %w", buildErr)
		}
		result.Metadata, result.RawMetadata = metadata, rawMetadata
	}

	usage.Success = true
	usage.Detail = fmt.Sprintf("gallery %d/%d file(s), %d bytes (%s)", len(meta.Files), len(entries), totalBytes, completeness.State)
	c.recordUsage(db, usage)
	return result, nil
}

// xMediaEntries resolves the tweet's own media.
//
// photos[] is a list of URL strings; videos[] is a list of objects
// ({video_url, duration}) whose entries are resolution variants of one video,
// so they collapse to the best one. Both shapes are read defensively — a field
// that is a string list in one sample and an object list in another is a
// property of provider records, not an accident.
//
// external_image_urls/external_video_urls and quoted_post media are
// deliberately excluded: they belong to a linked page or a different tweet, and
// counting them would both archive someone else's media and corrupt the
// completeness count for this one.
func xMediaEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	for _, u := range stringsFromField(record, "photos", "url", "image_url", "photo_url") {
		entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
	}
	for _, u := range bestVideoVariants(stringsFromField(record, "videos", "video_url", "url", "playback_url")) {
		entries = append(entries, mediaEntry{URL: u, Type: "Video"})
	}
	return entries
}

// singleXVideo returns the video URL when the tweet holds exactly one video.
func singleXVideo(entries []mediaEntry) string {
	if len(entries) == 1 && entries[0].isVideo() {
		return entries[0].URL
	}
	return ""
}

func xGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "twitter",
		Subcategory: "brightdata",
		PostID:      stringField(record, "id", "post_id"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(record, "user_posted"),
		AuthorName:  stringField(record, "name"),
		Description: stringField(record, "description"),
		Date:        stringField(record, "date_posted"),
		Tags:        stringSlice(record, "hashtags"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if likes := intField(record, "likes"); likes != nil {
		meta.Likes = likes
	}
	return meta
}

func buildBrightDataXVideoArtifacts(record map[string]any, sourceURL, videoURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data X record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data X record: %w", err)
	}

	// X reports a video's duration in milliseconds.
	var duration *float64
	if ms := firstObjectField(record, "videos", "duration", "duration_ms"); ms != nil && *ms > 0 {
		seconds := float64(*ms) / 1000
		duration = &seconds
	}

	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "x",
		Extractor:            "twitter",
		PostID:               stringField(record, "id", "post_id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Description:          stringField(record, "description"),
		Author:               stringField(record, "name", "user_posted"),
		AuthorID:             stringField(record, "user_id", "profile_id"),
		Uploader:             stringField(record, "user_posted"),
		UploaderID:           stringField(record, "user_id", "profile_id"),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "date_posted")),
		DurationSeconds:      duration,
		Engagement: archivers.VideoEngagement{
			Views:    intField(record, "views"),
			Likes:    intField(record, "likes"),
			Comments: intField(record, "replies"),
			Reposts:  intField(record, "reposts"),
		},
		Tags: stringSlice(record, "hashtags"),
		Media: archivers.VideoMedia{
			Extension:    ".mp4",
			ContentType:  "video/mp4",
			SizeBytes:    size,
			QualityLabel: videoQualityLabel(videoURL),
		},
		ArchivedAt: archivedAt.UTC().Format(time.RFC3339),
		Provenance: models.ArchiveSourceBrightData,
		Provider:   "brightdata_web_scraper",
	})
	if err != nil {
		return nil, nil, err
	}
	return &archivers.Sidecar{Data: metadataJSON}, &archivers.Sidecar{Data: sanitized}, nil
}

func logXMetadata(logWriter io.Writer, record map[string]any) {
	author := stringField(record, "user_posted")
	if name := stringField(record, "name"); name != "" && author != "" {
		fmt.Fprintf(logWriter, "Author: %s (@%s)\n", name, author)
	} else if author != "" {
		fmt.Fprintf(logWriter, "Author: @%s\n", author)
	}
	if text := stringField(record, "description"); text != "" {
		fmt.Fprintf(logWriter, "Text: %s\n", utils.TruncateForLog(text, 300))
	}
	if date := stringField(record, "date_posted"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	for _, counter := range []struct{ label, key string }{
		{"Views", "views"}, {"Likes", "likes"}, {"Reposts", "reposts"}, {"Replies", "replies"},
	} {
		if value := intField(record, counter.key); value != nil {
			fmt.Fprintf(logWriter, "%s: %d\n", counter.label, *value)
		}
	}
}
