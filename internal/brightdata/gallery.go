package brightdata

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"arker/internal/archivers"
	"arker/internal/models"
)

// mediaFetcher downloads one resolved media entry to dest and returns the
// number of bytes written. Platforms whose media Arker can fetch over its own
// connection pass Client.directFetch; TikTok wraps it with a Bright Data
// browser fallback for the assets its CDN refuses.
type mediaFetcher func(ctx context.Context, entry mediaEntry, dest string) (int64, error)

// buildGalleryArchive downloads every asset a post declares into a ZIP laid
// out exactly like the native gallery-dl artifact — numbered media files,
// Arker's normalized metadata.json, and the sanitized raw provider record — so
// the existing gallery viewer and API serve a Bright Data bundle with no
// changes.
//
// Completeness is measured against what the provider record said the post
// holds, never against what happened to download. That is the difference
// between "a three-image post" and "three images of a ten-image post", and it
// is the only reason a partial rescue cannot read green.
func (c *Client) buildGalleryArchive(ctx context.Context, entries []mediaEntry, meta *archivers.GalleryMetadata, record map[string]any, fetch mediaFetcher, logWriter io.Writer) (archivers.Result, archivers.Completeness, int64, error) {
	if len(entries) == 0 {
		return archivers.Result{}, archivers.Completeness{}, 0, fmt.Errorf("Bright Data record contains no media")
	}

	tmpDir, err := os.MkdirTemp("", "arker-bd-gallery-*")
	if err != nil {
		return archivers.Result{}, archivers.Completeness{}, 0, err
	}
	defer os.RemoveAll(tmpDir)

	var totalBytes int64
	var missing []int
	for i, entry := range entries {
		name := fmt.Sprintf("%03d%s", i+1, entry.extension())
		path := filepath.Join(tmpDir, name)
		fmt.Fprintf(logWriter, "Downloading %s...\n", name)
		size, err := fetch(ctx, entry, path)
		if err == nil {
			// A CDN that answers 200 with an error page would otherwise be
			// stored as the post's video and counted towards completeness —
			// a bundle that looks whole and holds an HTML document.
			err = verifyGalleryMedia(entry, path)
			if err != nil {
				removeFile(path)
			}
		}
		if err != nil {
			// A carousel with one dead slide is still worth archiving, but the
			// archive has to say which slide was lost instead of looking like a
			// shorter post.
			fmt.Fprintf(logWriter, "Failed to download %s: %v\n", name, err)
			missing = append(missing, i+1)
			continue
		}
		totalBytes += size
		meta.Files = append(meta.Files, archivers.GalleryFile{
			Name:        name,
			Size:        size,
			ContentType: entry.contentType(),
			IsVideo:     entry.isVideo(),
		})
	}
	if len(meta.Files) == 0 {
		return archivers.Result{}, archivers.Completeness{}, totalBytes,
			fmt.Errorf("all %d media download(s) failed", len(entries))
	}

	meta.FileCount = len(meta.Files)
	expected := len(entries)
	completeness := archivers.CompletenessFromCounts(&expected, len(meta.Files), false)
	completeness.MissingIndices = missing
	meta.Completeness = &completeness

	fmt.Fprintf(logWriter, "Downloaded %d of %d media file(s), %d bytes total\n", len(meta.Files), expected, totalBytes)

	zipPath, err := buildGalleryZip(tmpDir, meta, record)
	if err != nil {
		return archivers.Result{}, completeness, totalBytes, err
	}

	thumb := galleryThumbnailFromDir(tmpDir, meta, logWriter)

	reader, err := openTempFileReader(zipPath)
	if err != nil {
		removeFile(zipPath)
		return archivers.Result{}, completeness, totalBytes, err
	}
	return archivers.Result{
		Data:         reader,
		Extension:    ".zip",
		ContentType:  "application/zip",
		Thumbnail:    thumb,
		Source:       models.ArchiveSourceBrightData,
		Completeness: completeness.State,
	}, completeness, totalBytes, nil
}

// directFetch downloads one entry over Arker's own connection. It is the
// fetcher for every platform whose media URLs are portable — Instagram, Reddit
// and X sign their CDN URLs but do not bind them to the resolving IP, so only
// the resolution has to be paid for.
func (c *Client) directFetch(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
	return c.downloadToPath(ctx, entry.URL, dest)
}

// closeResultData releases an archive result the caller decided to abandon.
// The reader owns a temp file that nothing else will ever delete, so a result
// dropped on an error path has to be closed rather than garbage-collected.
func closeResultData(result archivers.Result) {
	if closer, ok := result.Data.(io.Closer); ok {
		closer.Close()
	}
}

// verifyGalleryMedia rejects a download that is not what the entry claimed to
// be. Only MP4/MOV containers are checkable this cheaply (both are ISO base
// media, so "ftyp" at offset 4 identifies them); images are left alone, since a
// broken one is visible rather than silently counted as the post's video.
func verifyGalleryMedia(entry mediaEntry, path string) error {
	switch entry.extension() {
	case ".mp4", ".mov":
		return verifyMP4(path)
	}
	return nil
}

// storedFileSize returns the size recorded for the first stored file matching
// pred, or 0. Used to report the video's own size in normalized metadata when
// the stored artifact is a ZIP around it.
func storedFileSize(meta *archivers.GalleryMetadata, pred func(archivers.GalleryFile) bool) int64 {
	for _, file := range meta.Files {
		if pred(file) {
			return file.Size
		}
	}
	return 0
}

// videoResolutionPatterns match the resolution marker platforms embed in a
// media URL, most specific first.
var videoResolutionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(\d{2,5})x(\d{2,5})$`),  // X: .../vid/avc1/3840x2160/name.mp4
	regexp.MustCompile(`(?i)res[_-]?(\d{2,5})p`), // Reddit: .../pb/m2-res_1080p.mp4
	regexp.MustCompile(`(?i)(?:^|[^0-9])(\d{3,4})p(?:$|[^0-9])`),
}

// bestVideoVariants collapses multi-resolution variants of the same video into
// one entry, keeping the highest resolution of each.
//
// Reddit and X both return a single video as a list of URLs that differ only in
// resolution (packaged-media.redd.it/<id>/pb/m2-res_{392..1920}p.mp4;
// video.twimg.com/amplify_video/<id>/vid/avc1/{640x360,3840x2160}/<name>.mp4).
// Storing all of them would archive the same video six times and inflate the
// expected asset count; storing the first would quietly archive a 392p copy of
// a 1080p video. Variants are grouped by the URL path up to the segment that
// carries the resolution, so a post that genuinely holds two different videos
// still yields two entries.
//
// Scores are only compared within a group, where every variant uses the same
// URL scheme, so the pixel-count and vertical-lines scales never meet.
func bestVideoVariants(urls []string) []string {
	type variant struct {
		url   string
		score int64
		order int
	}
	var groups []string
	best := map[string]variant{}

	for i, raw := range urls {
		key, score := videoVariantKey(raw)
		current, seen := best[key]
		if !seen {
			groups = append(groups, key)
			best[key] = variant{url: raw, score: score, order: i}
			continue
		}
		if score > current.score {
			best[key] = variant{url: raw, score: score, order: current.order}
		}
	}

	result := make([]string, 0, len(groups))
	for _, key := range groups {
		result = append(result, best[key].url)
	}
	return result
}

// videoVariantKey returns the grouping key and resolution score for one media
// URL. A URL with no recognizable resolution marker is its own group, scored
// zero, so nothing is ever silently dropped.
func videoVariantKey(rawURL string) (string, int64) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, 0
	}
	segments := strings.Split(parsed.Path, "/")
	for i, segment := range segments {
		if score, ok := videoResolutionScore(segment); ok {
			return parsed.Host + strings.Join(segments[:i], "/"), score
		}
	}
	return rawURL, 0
}

func videoResolutionScore(segment string) (int64, bool) {
	for _, pattern := range videoResolutionPatterns {
		m := pattern.FindStringSubmatch(segment)
		if m == nil {
			continue
		}
		if len(m) > 2 && m[2] != "" {
			width, _ := strconv.ParseInt(m[1], 10, 64)
			height, _ := strconv.ParseInt(m[2], 10, 64)
			return width * height, true
		}
		value, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		return value, true
	}
	return 0, false
}

// videoQualityLabel names the resolution a media URL advertises, for the
// normalized metadata's quality_label. Empty when the URL says nothing.
func videoQualityLabel(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if m := videoResolutionPatterns[0].FindStringSubmatch(segment); m != nil {
			return m[0]
		}
		if m := videoResolutionPatterns[1].FindStringSubmatch(segment); m != nil {
			return m[1] + "p"
		}
		if m := videoResolutionPatterns[2].FindStringSubmatch(segment); m != nil {
			return m[1] + "p"
		}
	}
	return ""
}

// stringsFromField reads a record field that may hold a list of URL strings or
// a list of objects carrying a URL under one of urlKeys.
//
// Provider records are not schemas: the same field can be a list of strings for
// one post type and a list of objects for another (X returns photos as strings
// and videos as {video_url, duration}), and a field that is null in every
// sample is not proof it is always null. Reading both shapes costs a few lines
// and turns a whole class of "worked on the fixture, crashed in production" into
// a no-op.
func stringsFromField(record map[string]any, key string, urlKeys ...string) []string {
	values, ok := record[key].([]any)
	if !ok {
		// A provider that returns a single value where the schema says list.
		if single, ok := record[key].(map[string]any); ok {
			if u := stringField(single, urlKeys...); u != "" {
				return []string{u}
			}
			return nil
		}
		if single := stringField(record, key); single != "" {
			return []string{single}
		}
		return nil
	}

	var result []string
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				result = append(result, trimmed)
			}
		case map[string]any:
			if u := stringField(typed, urlKeys...); u != "" {
				result = append(result, u)
			}
		}
	}
	return result
}

// firstObjectField reads a numeric field from the first object in a list-valued
// record field (X carries a video's duration there).
func firstObjectField(record map[string]any, key string, fieldKeys ...string) *int64 {
	values, ok := record[key].([]any)
	if !ok {
		return nil
	}
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			if found := intField(object, fieldKeys...); found != nil {
				return found
			}
		}
	}
	return nil
}
