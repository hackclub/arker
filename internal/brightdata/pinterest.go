package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// archivePinterest is the Pinterest fallback entry point.
//
// Pinterest shows a logged-out client a login wall instead of the pin, so the
// native gallery-dl run cannot succeed without a cookie jar — which is why a
// pin only gets an archive item at all when this fallback is configured
// (utils.ShouldCreateGalleryDLItem). Before that, a pin captured without
// cookies produced no media item whatsoever: an MHTML of the login wall and
// nothing else.
//
// The Posts dataset returns the pin plus image_video_url, and that URL is an
// ordinary i.pinimg.com asset which downloads straight from Arker's own
// connection (verified 206 + JPEG bytes), so only the resolution is paid for.
//
// Pinterest routes to gallery-dl only, so every rescue produces a gallery
// bundle; a video pin also gets the normalized video metadata sidecar, which is
// where the engagement counts and the publication timestamp live.
func (c *Client) archivePinterest(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no Pinterest fallback for archive type %s", itemType)
	}

	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetPinterestPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := pinterestMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no media (post_type %q)",
			targetURL, stringField(record, "post_type"))
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	logPinterestMetadata(logWriter, record)

	meta := pinterestGalleryMetadata(record, targetURL)
	result, completeness, totalBytes, err := c.buildGalleryArchive(ctx, entries, nil, meta, record, c.directFetch, logWriter)
	if err != nil {
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	// A video pin is one video and nothing else: describe it with the normalized
	// video contract as well, so the counts and the publication timestamp
	// survive in a machine-readable shape. GalleryMetadata has nowhere to put
	// them.
	if videoURL := singlePinterestVideo(entries); videoURL != "" {
		size := storedFileSize(meta, func(f archivers.GalleryFile) bool { return f.IsVideo })
		metadata, rawMetadata, buildErr := buildBrightDataPinterestVideoArtifacts(record, targetURL, videoURL, size, time.Now())
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

// pinterestMediaEntries resolves the pin's own media.
//
// A pin is a single asset: image_video_url is the file itself — an
// i.pinimg.com image for an image pin, an MP4 for a video pin — and
// attached_files repeats it in every verified record. The two are merged and
// deduplicated by URL rather than concatenated, because completeness is counted
// against this list: listing the same file twice would archive it twice and
// report a whole one-image pin as half-archived the moment one copy failed.
//
// A pin whose video is only published as an HLS playlist (.m3u8) is kept in the
// list on purpose. The playlist is not a video file, so the download is
// rejected by the container check and the archive says the asset is missing —
// which is the honest answer, and a far better one than a .mp4 holding a few
// hundred bytes of manifest text.
func pinterestMediaEntries(record map[string]any) []mediaEntry {
	postType := stringField(record, "post_type")

	var entries []mediaEntry
	seen := map[string]bool{}
	add := func(rawURL string) {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" || seen[rawURL] {
			return
		}
		seen[rawURL] = true
		entries = append(entries, mediaEntry{URL: rawURL, Type: pinterestEntryType(rawURL, postType)})
	}

	add(stringField(record, "image_video_url"))
	for _, u := range stringsFromField(record, "attached_files", "url", "file_url", "image_video_url") {
		add(u)
	}
	return entries
}

// pinterestEntryType classifies one pin asset.
//
// post_type names what the pin is ("image", "video", "story"), but a video
// pin's attached_files can still carry the poster image, so the URL's own
// extension decides whenever it says anything and post_type only breaks the
// tie.
func pinterestEntryType(rawURL, postType string) string {
	switch strings.ToLower(path.Ext(urlPath(rawURL))) {
	case ".mp4", ".mov", ".webm", ".m3u8":
		return "Video"
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return "Photo"
	}
	if strings.Contains(strings.ToLower(postType), "video") {
		return "Video"
	}
	return "Photo"
}

// singlePinterestVideo returns the video URL when the pin is exactly one video.
func singlePinterestVideo(entries []mediaEntry) string {
	if len(entries) == 1 && entries[0].isVideo() {
		return entries[0].URL
	}
	return ""
}

// pinterestGalleryMetadata maps the dataset record onto Arker's normalized
// gallery metadata, the same shape the native gallery-dl flow writes.
func pinterestGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "pinterest",
		Subcategory: "brightdata",
		PostID:      stringField(record, "post_id", "id"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(record, "user_name", "user"),
		Title:       stringField(record, "title"),
		Description: stringField(record, "content", "description"),
		Date:        stringField(record, "date_posted"),
		// Pinterest's "hashtags" are the pin's interest labels rather than
		// literal #tags, but they are the tag list the platform publishes and the
		// native extractor reports.
		Tags:        stringSlice(record, "hashtags"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if likes := intField(record, "likes"); likes != nil {
		meta.Likes = likes
	}
	return meta
}

func buildBrightDataPinterestVideoArtifacts(record map[string]any, sourceURL, videoURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data Pinterest record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data Pinterest record: %w", err)
	}

	// video_length is deliberately not reported as a duration: every verified
	// record is an image pin carrying 0, so whether the field counts seconds or
	// milliseconds is unknown, and a duration that is wrong by 1000x is worse
	// than one the archive does not claim. Filling it in is a one-line change
	// once a live video pin settles the unit.
	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "pinterest",
		Extractor:            "pinterest",
		PostID:               stringField(record, "post_id", "id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                stringField(record, "title"),
		Description:          stringField(record, "content", "description"),
		Author:               stringField(record, "user_name", "user"),
		AuthorID:             stringField(record, "user_id"),
		Uploader:             stringField(record, "user_name", "user"),
		UploaderID:           stringField(record, "user_id"),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "date_posted")),
		Engagement: archivers.VideoEngagement{
			Likes:    intField(record, "likes"),
			Comments: intField(record, "comments_num", "num_comments"),
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

// logPinterestMetadata writes the pin's human-relevant metadata into the
// archive log, where the native flows put theirs.
func logPinterestMetadata(logWriter io.Writer, record map[string]any) {
	if author := stringField(record, "user_name", "user"); author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", author)
	}
	if title := stringField(record, "title"); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", utils.TruncateForLog(title, 300))
	}
	if text := stringField(record, "content", "description"); text != "" {
		fmt.Fprintf(logWriter, "Description: %s\n", utils.TruncateForLog(text, 300))
	}
	if date := stringField(record, "date_posted"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if postType := stringField(record, "post_type"); postType != "" {
		fmt.Fprintf(logWriter, "Pin type: %s\n", postType)
	}
	if likes := intField(record, "likes"); likes != nil {
		fmt.Fprintf(logWriter, "Likes: %d\n", *likes)
	}
	if comments := intField(record, "comments_num", "num_comments"); comments != nil {
		fmt.Fprintf(logWriter, "Comments: %d\n", *comments)
	}
}
