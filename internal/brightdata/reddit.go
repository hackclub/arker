package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// archiveReddit is the Reddit fallback entry point.
//
// Reddit is not login-walled, so the native gallery-dl run is the normal path;
// what fails is positional — Reddit blocks its .json API from many datacenter
// ranges, and its video posts need gallery-dl's yt-dlp integration to mux the
// DASH audio and video back together. Bright Data sidesteps both: the Posts
// dataset returns packaged-media.redd.it URLs that are already muxed
// (video+audio in one MP4, verified) and download straight from Arker's own
// connection, so only the resolution is paid for.
//
// Reddit routes to gallery-dl only (utils.IsGalleryDLURL), including for video
// posts, so every Reddit rescue produces a gallery bundle. A single-video post
// additionally gets the normalized video metadata sidecar, which is where the
// engagement counts and the publication timestamp live: GalleryMetadata has
// nowhere to put them.
func (c *Client) archiveReddit(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no Reddit fallback for archive type %s", itemType)
	}

	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetRedditPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := redditMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no media (a text-only post has nothing to archive)", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	logRedditMetadata(logWriter, record)

	meta := redditGalleryMetadata(record, targetURL)
	result, completeness, totalBytes, err := c.buildGalleryArchive(ctx, entries, meta, record, c.directFetch, logWriter)
	if err != nil {
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	// One video and nothing else is a video post: describe it with the
	// normalized video contract as well, so upvotes, comment count and the
	// publication timestamp survive in a machine-readable shape.
	if videoURL := singleRedditVideo(entries); videoURL != "" {
		size := storedFileSize(meta, func(f archivers.GalleryFile) bool { return f.IsVideo })
		metadata, rawMetadata, buildErr := buildBrightDataRedditVideoArtifacts(record, targetURL, videoURL, size, time.Now())
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

// redditMediaEntries resolves the post's assets from the dataset record.
//
// videos[] holds the same video at several resolutions rather than several
// videos, so it collapses to the best one; photos[] is the image and gallery
// case, one entry per slide.
func redditMediaEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	for _, u := range bestVideoVariants(stringsFromField(record, "videos", "url", "video_url")) {
		entries = append(entries, mediaEntry{URL: u, Type: "Video"})
	}
	for _, u := range stringsFromField(record, "photos", "url", "image_url") {
		entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
	}
	return entries
}

// singleRedditVideo returns the video URL when the post is exactly one video.
func singleRedditVideo(entries []mediaEntry) string {
	if len(entries) == 1 && entries[0].isVideo() {
		return entries[0].URL
	}
	return ""
}

// redditGalleryMetadata maps the dataset record onto Arker's normalized gallery
// metadata, the same shape the native gallery-dl flow writes.
func redditGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "reddit",
		Subcategory: "brightdata",
		PostID:      stringField(record, "post_id", "id"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(record, "user_posted"),
		Title:       stringField(record, "title"),
		Description: stringField(record, "description", "description_markdown"),
		Date:        stringField(record, "date_posted"),
		Tags:        stringSlice(record, "tag"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	// Reddit's upvote count is the closest thing it has to a like count, and is
	// what the native reddit extractor reports as "score".
	if upvotes := intField(record, "num_upvotes", "score"); upvotes != nil {
		meta.Likes = upvotes
	}
	if community := subredditName(record); community != "" {
		meta.AuthorName = community
	}
	return meta
}

// subredditName renders the community as r/<name>, the form a reader expects.
func subredditName(record map[string]any) string {
	name := stringField(record, "community_name")
	if name == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(name), "r/") {
		return name
	}
	return "r/" + name
}

func buildBrightDataRedditVideoArtifacts(record map[string]any, sourceURL, videoURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data Reddit record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data Reddit record: %w", err)
	}

	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "reddit",
		Extractor:            "reddit",
		PostID:               stringField(record, "post_id", "id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                stringField(record, "title"),
		Description:          stringField(record, "description", "description_markdown"),
		Author:               stringField(record, "user_posted"),
		AuthorID:             stringField(record, "user_id"),
		Uploader:             stringField(record, "user_posted"),
		UploaderID:           stringField(record, "user_id"),
		Channel:              subredditName(record),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "date_posted")),
		Engagement: archivers.VideoEngagement{
			Likes:    intField(record, "num_upvotes", "score"),
			Comments: intField(record, "num_comments"),
		},
		Media: archivers.VideoMedia{
			Extension:   ".mp4",
			ContentType: "video/mp4",
			SizeBytes:   size,
			// packaged-media.redd.it serves one muxed MP4 per resolution, which
			// is exactly what makes it worth paying for: the native path has to
			// merge Reddit's separate DASH audio stream to get here.
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

// logRedditMetadata writes the post's human-relevant metadata into the archive
// log, where the native flows put theirs.
func logRedditMetadata(logWriter io.Writer, record map[string]any) {
	if community := subredditName(record); community != "" {
		fmt.Fprintf(logWriter, "Community: %s\n", community)
	}
	if author := stringField(record, "user_posted"); author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", author)
	}
	if title := stringField(record, "title"); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", utils.TruncateForLog(title, 300))
	}
	if date := stringField(record, "date_posted"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if upvotes := intField(record, "num_upvotes"); upvotes != nil {
		fmt.Fprintf(logWriter, "Upvotes: %d\n", *upvotes)
	}
	if comments := intField(record, "num_comments"); comments != nil {
		fmt.Fprintf(logWriter, "Comments: %d\n", *comments)
	}
}
