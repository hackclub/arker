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

// archiveFacebook is the Facebook fallback entry point.
//
// Facebook fails Arker positionally in both directions: it serves a logged-out
// client a login wall instead of a post, and it refuses yt-dlp from datacenter
// ranges outright. The Posts-by-post-URL dataset resolves either shape, and the
// media it returns downloads straight from Arker's own connection — both
// video.fbcdn.net assets and scontent images (verified 206 + real MP4/JPEG
// bytes) — so only the resolution is paid for.
//
// Facebook is the one platform that arrives on both routes. Video permalinks
// (/reel/, /videos/, /watch, fb.watch) are yt-dlp items and produce an MP4;
// photo posts and post permalinks are gallery-dl items and produce a bundle.
// The second route is not only photos: a /posts/ permalink can wrap a video,
// so that flow handles both media classes and gives a single-video post the
// normalized video sidecar as well.
func (c *Client) archiveFacebook(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	switch {
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeYtDlp):
		return c.facebookVideo(ctx, targetURL, logWriter, db, itemID, shortID)
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl):
		return c.facebookGallery(ctx, targetURL, logWriter, db, itemID, shortID)
	default:
		return archivers.Result{}, fmt.Errorf("no Facebook fallback for archive type %s", itemType)
	}
}

// facebookVideo produces the MP4 for a Facebook video permalink.
func (c *Client) facebookVideo(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetFacebookPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := facebookMediaEntries(record, logWriter)
	videoURL := firstFacebookVideo(entries)
	if videoURL == "" {
		err := fmt.Errorf("Bright Data record for %s has no video URL (post_type %q, %d media attachment(s))",
			targetURL, stringField(record, "post_type"), len(entries))
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	logFacebookMetadata(logWriter, record)
	fmt.Fprintf(logWriter, "Bright Data resolved the video URL; downloading from Facebook's CDN...\n")

	videoPath, size, err := c.downloadToTemp(ctx, videoURL, "arker-bd-fb-*.mp4")
	if err != nil {
		err = fmt.Errorf("video download failed: %w", err)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	if err := verifyMP4(videoPath); err != nil {
		removeFile(videoPath)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	fmt.Fprintf(logWriter, "Downloaded %d bytes of video via Bright Data fallback\n", size)

	metadata, rawMetadata, err := buildBrightDataFacebookVideoArtifacts(record, targetURL, videoURL, size, time.Now())
	if err != nil {
		removeFile(videoPath)
		usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, fmt.Errorf("failed to build Bright Data video metadata: %w", err)
	}

	reader, err := openTempFileReader(videoPath)
	if err != nil {
		removeFile(videoPath)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	usage.Success = true
	usage.Detail = fmt.Sprintf("video %d bytes", size)
	c.recordUsage(db, usage)
	return archivers.Result{
		Data:        reader,
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Thumbnail:   c.thumbnailFromURL(ctx, facebookPosterURL(record), logWriter),
		Source:      models.ArchiveSourceBrightData,
		Metadata:    metadata,
		RawMetadata: rawMetadata,
		// One video permalink is one video: the MP4 and both sidecars are
		// stored, so there is no second asset this could have missed.
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// facebookGallery produces a gallery bundle for a photo post or post permalink.
//
// A post permalink is a container for whatever the poster attached, which is
// usually photos but can be a video, so both media classes go through the same
// bundle; a post that turns out to be exactly one video also gets the
// normalized video contract, which is where the counts and the publication
// timestamp live.
func (c *Client) facebookGallery(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetFacebookPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := facebookMediaEntries(record, logWriter)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no media (post_type %q; a text-only post has nothing to archive)",
			targetURL, stringField(record, "post_type"))
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	logFacebookMetadata(logWriter, record)

	meta := facebookGalleryMetadata(record, targetURL)
	result, completeness, totalBytes, err := c.buildGalleryArchive(ctx, entries, meta, record, c.directFetch, logWriter)
	if err != nil {
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	if result.Thumbnail == nil {
		// A /posts/ permalink can contain one video. Its attachment publishes a
		// separate cover image just like the dedicated video route, so do not
		// fall back to a screenshot merely because the bundle has no still card.
		result.Thumbnail = c.thumbnailFromURL(ctx, facebookPosterURL(record), logWriter)
	}

	if videoURL := singleFacebookVideo(entries); videoURL != "" {
		size := storedFileSize(meta, func(f archivers.GalleryFile) bool { return f.IsVideo })
		metadata, rawMetadata, buildErr := buildBrightDataFacebookVideoArtifacts(record, targetURL, videoURL, size, time.Now())
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

// facebookMediaEntries resolves the post's own media from attachments[].
//
// Each attachment carries its bytes in a different field depending on its
// class, and reading the wrong one is not a near miss: a video attachment's
// `url` is the post's own page link, so a flow that read it would download an
// HTML document and store it as the post's video. Videos are therefore taken
// from video_url only, and photos from `url` (an scontent image), with
// thumbnail_url as the fallback.
//
// Attachments that are not the post's media are skipped and named in the log:
// an "audio" attachment is the DASH audio stream Facebook packages alongside a
// video, which downloads as a second MP4 and would be counted as a second asset
// of a single-video post.
//
// A record with no attachments still exposes a single image through post_image,
// which is the shape a plain photo post arrives in.
func facebookMediaEntries(record map[string]any, logWriter io.Writer) []mediaEntry {
	var entries []mediaEntry
	seen := map[string]bool{}
	add := func(rawURL, entryType string) {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" || seen[rawURL] {
			return
		}
		seen[rawURL] = true
		entries = append(entries, mediaEntry{URL: rawURL, Type: entryType})
	}

	attachments, _ := record["attachments"].([]any)
	for i, raw := range attachments {
		attachment, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		class := strings.ToLower(stringField(attachment, "type", "source_type"))
		switch {
		case strings.Contains(class, "video"):
			videoURL := stringField(attachment, "video_url")
			if videoURL == "" {
				fmt.Fprintf(logWriter, "Attachment %d is a video with no downloadable URL in the Bright Data record; skipping\n", i+1)
				continue
			}
			add(videoURL, "Video")
		case strings.Contains(class, "photo") || strings.Contains(class, "image"):
			add(firstNonEmptyString(stringField(attachment, "url"), stringField(attachment, "thumbnail_url")), "Photo")
		default:
			fmt.Fprintf(logWriter, "Attachment %d is %q, not the post's own media; skipping\n", i+1, class)
		}
	}

	if len(entries) == 0 {
		add(stringField(record, "post_image"), "Photo")
	}
	return entries
}

// firstFacebookVideo returns the first video in the post, for the yt-dlp route.
func firstFacebookVideo(entries []mediaEntry) string {
	for _, entry := range entries {
		if entry.isVideo() {
			return entry.URL
		}
	}
	return ""
}

// singleFacebookVideo returns the video URL when the post is exactly one video.
func singleFacebookVideo(entries []mediaEntry) string {
	if len(entries) == 1 && entries[0].isVideo() {
		return entries[0].URL
	}
	return ""
}

// facebookPosterURL is the post's cover image: a video attachment's thumbnail,
// or the post image of a photo post.
func facebookPosterURL(record map[string]any) string {
	attachments, _ := record["attachments"].([]any)
	for _, raw := range attachments {
		if attachment, ok := raw.(map[string]any); ok {
			if thumb := stringField(attachment, "thumbnail_url"); thumb != "" {
				return thumb
			}
		}
	}
	return stringField(record, "post_image")
}

// facebookLikeCount reads the post's like count.
//
// `likes` is the provider's own headline number and is preferred. The
// num_likes_type breakdown is only summed when `likes` is absent, and it
// arrives in two shapes — a list of {type, num} objects on a per-post record, a
// single {type, num} object on a page listing — so both are read. The two
// numbers do not agree in verified records (a per-post record's `likes` counts
// only the Like reaction while the breakdown totals every reaction), which is
// exactly why the headline number is not recomputed from the parts.
func facebookLikeCount(record map[string]any) *int64 {
	if likes := intField(record, "likes"); likes != nil {
		return likes
	}
	switch breakdown := record["num_likes_type"].(type) {
	case []any:
		var total int64
		found := false
		for _, raw := range breakdown {
			reaction, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if num := intField(reaction, "num"); num != nil {
				total += *num
				found = true
			}
		}
		if found {
			return &total
		}
	case map[string]any:
		return intField(breakdown, "num")
	}
	return nil
}

// facebookAuthorName is the page's display name; facebookAuthorHandle is its
// short handle. A Facebook post is published by a Page, so the two together are
// the equivalent of X's name and @handle.
func facebookAuthorName(record map[string]any) string {
	return stringField(record, "page_name", "user_username_raw")
}

func facebookAuthorHandle(record map[string]any) string {
	return stringField(record, "user_handle", "profile_handle")
}

// facebookGalleryMetadata maps the dataset record onto Arker's normalized
// gallery metadata, the same shape the native gallery-dl flow writes.
func facebookGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "facebook",
		Subcategory: "brightdata",
		PostID:      stringField(record, "post_id", "shortcode"),
		PostURL:     stringField(record, "url"),
		Author:      firstNonEmptyString(facebookAuthorHandle(record), facebookAuthorName(record)),
		AuthorName:  facebookAuthorName(record),
		Title:       stringField(record, "video_title"),
		Description: stringField(record, "content"),
		Date:        stringField(record, "date_posted"),
		Tags:        stringSlice(record, "hashtags"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	meta.Likes = facebookLikeCount(record)
	return meta
}

func buildBrightDataFacebookVideoArtifacts(record map[string]any, sourceURL, videoURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data Facebook record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data Facebook record: %w", err)
	}

	// A video attachment's video_length is null in every verified record, so
	// whether it counts seconds or milliseconds is unknown and no duration is
	// claimed. A duration wrong by 1000x is worse than an absent one.
	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "facebook",
		Extractor:            "facebook",
		PostID:               stringField(record, "post_id", "shortcode"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                stringField(record, "video_title"),
		Description:          stringField(record, "content"),
		Author:               facebookAuthorName(record),
		AuthorID:             stringField(record, "profile_id", "delegate_page_id"),
		Uploader:             facebookAuthorHandle(record),
		UploaderID:           stringField(record, "profile_id", "delegate_page_id"),
		Channel:              facebookAuthorName(record),
		ChannelID:            stringField(record, "profile_id"),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "date_posted")),
		Engagement: archivers.VideoEngagement{
			// video_view_count is Facebook's own "views"; play_count counts
			// plays, which is a larger and different number, so it is only a
			// fallback rather than a substitute.
			Views:    intField(record, "video_view_count", "play_count"),
			Likes:    facebookLikeCount(record),
			Comments: intField(record, "num_comments"),
			Reposts:  intField(record, "num_shares"),
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

// logFacebookMetadata writes the post's human-relevant metadata into the
// archive log, where the native flows put theirs.
func logFacebookMetadata(logWriter io.Writer, record map[string]any) {
	if page := facebookAuthorName(record); page != "" {
		fmt.Fprintf(logWriter, "Page: %s\n", page)
	}
	if handle := facebookAuthorHandle(record); handle != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", handle)
	}
	if title := stringField(record, "video_title"); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", utils.TruncateForLog(title, 300))
	}
	if text := stringField(record, "content"); text != "" {
		fmt.Fprintf(logWriter, "Text: %s\n", utils.TruncateForLog(text, 300))
	}
	if date := stringField(record, "date_posted"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if views := intField(record, "video_view_count"); views != nil {
		fmt.Fprintf(logWriter, "Views: %d\n", *views)
	}
	if likes := facebookLikeCount(record); likes != nil {
		fmt.Fprintf(logWriter, "Likes: %d\n", *likes)
	}
	for _, counter := range []struct{ label, key string }{
		{"Comments", "num_comments"}, {"Shares", "num_shares"},
	} {
		if value := intField(record, counter.key); value != nil {
			fmt.Fprintf(logWriter, "%s: %d\n", counter.label, *value)
		}
	}
}
