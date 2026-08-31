package brightdata

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

// archiveInstagram is the Instagram fallback entry point. It resolves the post
// through Bright Data's Web Scraper API and downloads the media from
// Instagram's CDN directly: those URLs are signed but not IP-locked, so the
// bytes flow over Arker's own connection and only the resolution is paid for.
func (c *Client) archiveInstagram(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	switch itemType {
	case utils.ArchiveTypeYtDlp:
		return c.instagramVideo(ctx, targetURL, logWriter, db, itemID, shortID)
	case utils.ArchiveTypeGalleryDl:
		return c.instagramGallery(ctx, targetURL, logWriter, db, itemID, shortID)
	default:
		return archivers.Result{}, fmt.Errorf("no Instagram fallback for archive type %s", itemType)
	}
}

// instagramVideo produces the MP4 for a reel or a video feed post.
//
// Reels go to the reels dataset, whose video_url is a complete muxed MP4
// (verified: h264+aac in one file, no separate audio merge needed). Feed posts
// (/p/, /tv/) go to the posts dataset, which models videos as entries in the
// post's media list. A reel the reels dataset cannot resolve is retried
// against the posts dataset before giving up: the two scrapers have different
// blind spots and a second record costs a tenth of a cent.
func (c *Client) instagramVideo(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	lower := strings.ToLower(targetURL)
	datasets := []string{DatasetInstagramPosts}
	if strings.Contains(lower, "/reel/") {
		datasets = []string{DatasetInstagramReels, DatasetInstagramPosts}
	}

	var lastErr error
	for _, datasetID := range datasets {
		usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
		record, err := c.resolveRecord(ctx, db, usage, datasetID, targetURL, logWriter)
		if err != nil {
			lastErr = err
			continue
		}

		videoURL := firstNonEmptyString(
			stringField(record, "video_url"),
			firstVideoFromPost(record),
		)
		if videoURL == "" {
			lastErr = fmt.Errorf("Bright Data record for %s has no video (dataset %s)", targetURL, datasetID)
			usage.Detail = truncate(lastErr.Error(), 500)
			c.recordUsage(db, usage)
			continue
		}

		fmt.Fprintf(logWriter, "Bright Data resolved video URL; downloading from Instagram CDN...\n")
		videoPath, size, err := c.downloadToTemp(ctx, videoURL, "arker-bd-ig-*.mp4")
		if err != nil {
			lastErr = fmt.Errorf("video download failed: %w", err)
			usage.Detail = truncate(lastErr.Error(), 500)
			c.recordUsage(db, usage)
			continue
		}
		if err := verifyMP4(videoPath); err != nil {
			removeFile(videoPath)
			lastErr = err
			usage.Detail = truncate(lastErr.Error(), 500)
			c.recordUsage(db, usage)
			continue
		}

		logInstagramMetadata(logWriter, record)
		fmt.Fprintf(logWriter, "Downloaded %d bytes of video via Bright Data fallback\n", size)

		usage.Success = true
		usage.Detail = fmt.Sprintf("video %d bytes", size)

		metadata, rawMetadata, err := buildBrightDataInstagramVideoArtifacts(record, targetURL, size, time.Now())
		if err != nil {
			removeFile(videoPath)
			usage.Success = false
			usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
			c.recordUsage(db, usage)
			return archivers.Result{}, fmt.Errorf("failed to build Bright Data video metadata: %w", err)
		}

		reader, err := openTempFileReader(videoPath)
		if err != nil {
			removeFile(videoPath)
			usage.Success = false
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return archivers.Result{}, err
		}
		c.recordUsage(db, usage)
		return archivers.Result{
			Data:        reader,
			Extension:   ".mp4",
			ContentType: "video/mp4",
			Thumbnail:   c.thumbnailFromURL(ctx, stringField(record, "thumbnail"), logWriter),
			Source:      models.ArchiveSourceBrightData,
			Metadata:    metadata,
			RawMetadata: rawMetadata,
			// One reel is one video: the muxed MP4 and both sidecars are stored,
			// so there is no second asset this could have missed.
			Completeness: archivers.CompletenessComplete,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Bright Data dataset could resolve %s", targetURL)
	}
	return archivers.Result{}, lastErr
}

func buildBrightDataInstagramVideoArtifacts(record map[string]any, sourceURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data Instagram record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data Instagram record: %w", err)
	}

	// "length" is what the reels dataset actually calls its duration (a float,
	// 68.475647 for a 68-second reel); the posts dataset uses "videos_duration".
	// The original video_duration/duration keys exist in no observed Instagram
	// record, which is why brightdata-provenance reels shipped without a
	// duration until this was checked against real snapshots. The stored-bytes
	// ffprobe backfill overrides this where it runs; the record value is the
	// fallback that keeps the fact when probing is unavailable.
	duration := floatField(record, "length", "video_duration", "videos_duration", "duration")
	if duration != nil && *duration <= 0 {
		// A zero is what these datasets write for "no duration", not a fact.
		duration = nil
	}
	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "instagram",
		Extractor:            "instagram",
		PostID:               stringField(record, "shortcode", "post_id", "content_id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                instagramVideoTitle(record),
		Description:          stringField(record, "description", "caption"),
		Author:               stringField(record, "user_posted", "username", "owner_username"),
		AuthorID:             stringField(record, "user_id", "owner_id"),
		Uploader:             stringField(record, "user_posted", "username", "owner_username"),
		UploaderID:           stringField(record, "user_id", "owner_id"),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "date_posted", "timestamp")),
		DurationSeconds:      duration,
		Engagement: archivers.VideoEngagement{
			Views:    intField(record, "video_view_count", "views", "view_count", "video_play_count"),
			Likes:    intField(record, "likes", "like_count"),
			Comments: intField(record, "num_comments", "comments", "comment_count"),
		},
		Tags: stringSlice(record, "hashtags"),
		Media: archivers.VideoMedia{
			Extension:   ".mp4",
			ContentType: "video/mp4",
			SizeBytes:   size,
			Width:       intField(record, "video_width", "width"),
			Height:      intField(record, "video_height", "height"),
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

// instagramGallery produces a gallery ZIP for a feed post, in the same layout
// GalleryDLArchiver writes (numbered media files plus metadata.json), so the
// existing gallery viewer and API serve it with no changes. The raw Bright
// Data record is preserved as brightdata.json alongside it, mirroring how the
// native flow keeps gallery-dl's sidecars.
func (c *Client) instagramGallery(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetInstagramPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := postMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no media", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	tmpDir, err := os.MkdirTemp("", "arker-bd-gallery-*")
	if err != nil {
		return archivers.Result{}, err
	}
	defer os.RemoveAll(tmpDir)

	meta := galleryMetadataFromRecord(record, targetURL)
	var totalBytes int64
	var missing []int
	for i, entry := range entries {
		name := fmt.Sprintf("%03d%s", i+1, entry.extension())
		fmt.Fprintf(logWriter, "Downloading %s from Instagram CDN...\n", name)
		size, err := c.downloadToPath(ctx, entry.URL, filepath.Join(tmpDir, name))
		if err != nil {
			// A carousel with one dead slide is still worth archiving, but a
			// post where nothing downloads is not. Record which slide was lost
			// so the archive says so instead of looking like a shorter post.
			fmt.Fprintf(logWriter, "Failed to download %s: %v\n", name, err)
			missing = append(missing, i+1)
			continue
		}
		name, contentType, err := canonicalizeDownloadedGalleryMedia(tmpDir, name, logWriter)
		if err != nil {
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return archivers.Result{}, err
		}
		totalBytes += size
		meta.Files = append(meta.Files, archivers.GalleryFile{
			Name:        name,
			Size:        size,
			ContentType: contentType,
			IsVideo:     strings.HasPrefix(contentType, "video/"),
		})
	}
	if len(meta.Files) == 0 {
		err := fmt.Errorf("all media downloads failed for %s", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	meta.FileCount = len(meta.Files)
	// The dataset record lists every asset in the post, so a dead CDN slide is
	// a knowable gap rather than an invisible one: expected is what Bright Data
	// said the post holds, stored is what actually downloaded.
	expected := len(entries)
	completeness := archivers.CompletenessFromCounts(&expected, len(meta.Files), false)
	completeness.MissingIndices = missing
	meta.Completeness = &completeness

	// The Instagram datasets expose no per-slide duration or dimensions, so
	// the downloaded bytes are the only source for them — and this is the last
	// moment they exist as probeable files rather than an immutable stored ZIP.
	archivers.ProbeGalleryVideoFiles(ctx, tmpDir, meta.Files, logWriter)

	logInstagramMetadata(logWriter, record)
	fmt.Fprintf(logWriter, "Downloaded %d of %d media file(s), %d bytes total\n", len(meta.Files), len(entries), totalBytes)

	zipPath, err := buildGalleryZip(tmpDir, meta, record)
	if err != nil {
		usage.Detail = truncate("zip build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	usage.Success = true
	usage.Detail = fmt.Sprintf("gallery %d file(s), %d bytes", len(meta.Files), totalBytes)
	c.recordUsage(db, usage)

	thumb := galleryThumbnailFromDir(tmpDir, meta, logWriter)
	if thumb == nil {
		// /p/ URLs use the gallery route even when the post is one video. The
		// dataset publishes the post's real cover separately from that MP4; it
		// is the only correct preview for an all-video bundle (and is exactly
		// what capture mrQQB was missing).
		thumb = c.thumbnailFromURL(ctx, stringField(record, "thumbnail"), logWriter)
	}

	reader, err := openTempFileReader(zipPath)
	if err != nil {
		removeFile(zipPath)
		return archivers.Result{}, err
	}
	return archivers.Result{
		Data:         reader,
		Extension:    ".zip",
		ContentType:  "application/zip",
		Thumbnail:    thumb,
		Source:       models.ArchiveSourceBrightData,
		Completeness: completeness.State,
	}, nil
}

// resolveRecord runs a dataset collection and returns the first usable record.
func (c *Client) resolveRecord(ctx context.Context, db *gorm.DB, usage *models.BrightDataUsage, datasetID, targetURL string, logWriter io.Writer) (map[string]any, error) {
	records, err := c.runDataset(ctx, db, usage, datasetID, targetURL, logWriter)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if errMsg := stringField(record, "error"); errMsg != "" {
			err := fmt.Errorf("Bright Data returned an error record: %s", errMsg)
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return nil, err
		}
		return record, nil
	}
	err = fmt.Errorf("Bright Data snapshot contained no records for %s", targetURL)
	usage.Detail = truncate(err.Error(), 500)
	c.recordUsage(db, usage)
	return nil, err
}

// downloadToPath streams a URL to a specific path.
func (c *Client) downloadToPath(ctx context.Context, mediaURL, dest string) (int64, error) {
	tmpPath, size, err := c.downloadToTemp(ctx, mediaURL, "arker-bd-media-*")
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		removeFile(tmpPath)
		return 0, err
	}
	return size, nil
}

// mediaEntry is one media slide resolved from a post record.
type mediaEntry struct {
	URL  string
	Type string // "Photo" | "Video"
}

func (e mediaEntry) isVideo() bool {
	return strings.EqualFold(e.Type, "video") || strings.EqualFold(e.Type, "reel")
}

func (e mediaEntry) extension() string {
	if ext := strings.ToLower(path.Ext(urlPath(e.URL))); ext != "" && len(ext) <= 5 {
		switch ext {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic", ".mp4", ".mov", ".webm":
			return ext
		}
	}
	if e.isVideo() {
		return ".mp4"
	}
	return ".jpg"
}

// postMediaEntries extracts the ordered media list from a posts-dataset record.
// post_content carries {index, type, url} per slide; photos/videos are the
// flat fallbacks some records use instead.
func postMediaEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	if content, ok := record["post_content"].([]any); ok {
		for _, raw := range content {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			u := stringField(item, "url")
			if u == "" {
				continue
			}
			entries = append(entries, mediaEntry{URL: u, Type: stringField(item, "type")})
		}
	}
	if len(entries) > 0 {
		return entries
	}

	for _, u := range stringSlice(record, "photos") {
		entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
	}
	for _, u := range stringSlice(record, "videos") {
		entries = append(entries, mediaEntry{URL: u, Type: "Video"})
	}
	if len(entries) == 0 {
		if u := stringField(record, "video_url"); u != "" {
			entries = append(entries, mediaEntry{URL: u, Type: "Video"})
		}
	}
	return entries
}

// firstVideoFromPost returns the first video URL in a posts-dataset record.
func firstVideoFromPost(record map[string]any) string {
	for _, entry := range postMediaEntries(record) {
		if entry.isVideo() {
			return entry.URL
		}
	}
	return ""
}

// Instagram's datasets do not publish a title field for videos. yt-dlp's
// normalized record names the same kind of post "Video by <uploader>", so use
// that stable display title only when the record proves it is exactly one
// video. Photo posts and carousels stay untitled rather than being guessed.
func instagramVideoTitle(record map[string]any) string {
	if title := stringField(record, "title"); title != "" {
		return title
	}
	entries := postMediaEntries(record)
	if len(entries) != 1 || !entries[0].isVideo() {
		return ""
	}
	// The posts dataset's accessibility text starts with the display-name
	// title Instagram itself exposes ("Video by Anna Codes on August 19, ...").
	// Keep only that title prefix; the rest is an accessibility description,
	// not part of the title.
	if alt := strings.TrimSpace(stringField(record, "alt_text")); strings.HasPrefix(alt, "Video by ") {
		if end := strings.Index(alt, " on "); end > len("Video by ") {
			return alt[:end]
		}
	}
	author := stringField(record, "user_posted", "username", "owner_username")
	if author == "" {
		return ""
	}
	return "Video by " + author
}

// galleryMetadataFromRecord maps a Bright Data post record onto Arker's
// normalized gallery metadata, the same shape the native gallery-dl flow
// writes, so both flows are indistinguishable to the viewer.
func galleryMetadataFromRecord(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "instagram",
		Subcategory: "brightdata",
		PostID:      stringField(record, "shortcode", "post_id", "content_id"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(record, "user_posted"),
		Title:       instagramVideoTitle(record),
		Description: stringField(record, "description"),
		Date:        stringField(record, "date_posted", "timestamp"),
		Tags:        stringSlice(record, "hashtags"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if likes := intField(record, "likes"); likes != nil {
		meta.Likes = likes
	}
	meta.Views = intField(record, "video_view_count", "views", "video_play_count")
	meta.Comments = intField(record, "num_comments")
	return meta
}

// buildGalleryZip writes metadata.json, the raw record, and the media files
// into a ZIP laid out exactly like the native gallery-dl artifact.
//
// The raw record is sanitized on the way in, not on the way out. The bundle is
// downloadable, so a signed CDN URL written into it is a credential stored at
// rest — serve-time redaction would not reach it.
func buildGalleryZip(dir string, meta *archivers.GalleryMetadata, record map[string]any) (string, error) {
	metadataJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	rawJSON, err := archivers.SanitizeJSON(recordJSON, nil)
	if err != nil {
		return "", fmt.Errorf("sanitize Bright Data record: %w", err)
	}

	zipFile, err := createTempFile("arker-bd-gallery-*.zip")
	if err != nil {
		return "", err
	}
	zipPath := zipFile.Name()

	ok := false
	defer func() {
		if !ok {
			removeFile(zipPath)
		}
	}()

	zw := zip.NewWriter(zipFile)
	writeEntry := func(name string, method uint16, content func(io.Writer) error) error {
		header := &zip.FileHeader{Name: name, Method: method}
		header.SetModTime(time.Now())
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		return content(w)
	}

	// metadata.json first so a streaming reader finds it without scanning.
	if err := writeEntry("metadata.json", zip.Deflate, func(w io.Writer) error {
		_, err := w.Write(metadataJSON)
		return err
	}); err != nil {
		zw.Close()
		zipFile.Close()
		return "", err
	}
	if err := writeEntry("brightdata.json", zip.Deflate, func(w io.Writer) error {
		_, err := w.Write(rawJSON)
		return err
	}); err != nil {
		zw.Close()
		zipFile.Close()
		return "", err
	}

	for _, file := range meta.Files {
		// Media is stored uncompressed, matching the native flow: JPEG/MP4
		// does not deflate meaningfully.
		if err := writeEntry(file.Name, zip.Store, func(w io.Writer) error {
			f, err := os.Open(filepath.Join(dir, file.Name))
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		}); err != nil {
			zw.Close()
			zipFile.Close()
			return "", err
		}
	}

	if err := zw.Close(); err != nil {
		zipFile.Close()
		return "", err
	}
	if err := zipFile.Close(); err != nil {
		return "", err
	}
	ok = true
	return zipPath, nil
}

// galleryThumbnailFromDir previews the post from its first still image,
// matching the native gallery flow's choice of cover.
func galleryThumbnailFromDir(dir string, meta *archivers.GalleryMetadata, logWriter io.Writer) *archivers.Thumbnail {
	for _, file := range meta.Files {
		if file.IsVideo || !strings.HasPrefix(file.ContentType, "image/") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, file.Name))
		if err != nil {
			continue
		}
		t, err := thumbnail.OriginalFromReader(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(logWriter, "Thumbnail from %s skipped: %v\n", file.Name, err)
			continue
		}
		return &archivers.Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialOriginal}
	}
	return nil
}

// thumbnailFromURL downloads and retains a provider's poster image as the item
// thumbnail. Failures return nil: previews are cosmetic.
func (c *Client) thumbnailFromURL(ctx context.Context, imageURL string, logWriter io.Writer) *archivers.Thumbnail {
	thumb, err := c.thumbnailFromURLStrict(ctx, imageURL, logWriter)
	if err != nil {
		fmt.Fprintf(logWriter, "Could not download poster image: %v\n", err)
		return nil
	}
	return thumb
}

// thumbnailFromURLStrict is the error-preserving form used by the historical
// backfill. Capture-time callers intentionally treat a missing poster as
// cosmetic and use thumbnailFromURL; a backfill needs the error so River can
// retry a transient CDN failure instead of incorrectly marking the post as
// permanently unavailable.
func (c *Client) thumbnailFromURLStrict(ctx context.Context, imageURL string, logWriter io.Writer) (*archivers.Thumbnail, error) {
	if imageURL == "" {
		return nil, fmt.Errorf("%w: provider record has no poster URL", archivers.ErrSocialThumbnailUnavailable)
	}
	tmpPath, _, err := c.downloadToTemp(ctx, imageURL, "arker-bd-thumb-*")
	if err != nil {
		return nil, err
	}
	defer removeFile(tmpPath)

	f, err := os.Open(tmpPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t, err := thumbnail.OriginalFromReader(f)
	if err != nil {
		return nil, fmt.Errorf("decode provider poster: %w", err)
	}
	fmt.Fprintf(logWriter, "Thumbnail captured: %dx%d, %d bytes\n", t.Width, t.Height, len(t.Data))
	return &archivers.Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialOriginal}, nil
}

// logInstagramMetadata writes the post's human-relevant metadata into the
// archive log, where the native flows put theirs.
func logInstagramMetadata(logWriter io.Writer, record map[string]any) {
	if author := stringField(record, "user_posted"); author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", author)
	}
	if desc := stringField(record, "description"); desc != "" {
		fmt.Fprintf(logWriter, "Caption: %s\n", utils.TruncateForLog(desc, 300))
	}
	if date := stringField(record, "date_posted"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if likes := intField(record, "likes"); likes != nil {
		fmt.Fprintf(logWriter, "Likes: %d\n", *likes)
	}
}

// verifyMP4 rejects downloads that are not actually MP4 containers (an HTML
// error page saved as .mp4 would otherwise be archived as a "success").
func verifyMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, 12)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("downloaded video is too short to be an MP4")
	}
	if string(header[4:8]) != "ftyp" {
		return fmt.Errorf("downloaded video is not an MP4 container")
	}
	return nil
}

func urlPath(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return parsed.Path
}

func stringField(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := record[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func stringSlice(record map[string]any, key string) []string {
	values, ok := record[key].([]any)
	if !ok {
		return nil
	}
	var result []string
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			result = append(result, strings.TrimSpace(s))
		}
	}
	return result
}

// intField reads a counter from a provider record.
//
// Numbers arrive as strings often enough to matter — TikTok's share_count is
// "99" while its digg_count is 1807, in the same record — and a reader that
// only accepts JSON numbers drops those silently, which shows up as an archive
// that reports no shares rather than as an error. Strings are parsed strictly:
// a value like "1.2K" is not a count this can honestly report.
func intField(record map[string]any, keys ...string) *int64 {
	for _, key := range keys {
		switch value := record[key].(type) {
		case float64:
			v := int64(value)
			return &v
		case json.Number:
			if v, err := value.Int64(); err == nil {
				return &v
			}
		case string:
			if v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				return &v
			}
		}
	}
	return nil
}

// floatField reads a fractional quantity (a duration in seconds) from a
// provider record. Records are not schemas: the reels dataset's "length" is a
// scalar, while the posts dataset's "videos_duration" is a one-element list of
// {url, video_duration} objects for a single-video post. Parsing is strict for
// the same reason intField's is: "1.2K" is not a number this can honestly
// report.
func floatField(record map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		if v := floatValue(record[key]); v != nil {
			return v
		}
	}
	return nil
}

func floatValue(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case json.Number:
		if v, err := typed.Float64(); err == nil {
			return &v
		}
	case string:
		if v, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return &v
		}
	case map[string]any:
		return floatField(typed, "video_duration", "duration", "length")
	case []any:
		if len(typed) == 1 {
			return floatValue(typed[0])
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
