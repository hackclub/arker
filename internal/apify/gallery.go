package apify

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"arker/internal/archivers"
	"arker/internal/models"
)

// mediaFetcher downloads one resolved media entry to dest and returns the
// number of bytes written. Client.directFetch serves every platform: CDN
// assets go out bare, and assets the actor stored in its key-value store go
// out with the token.
type mediaFetcher func(ctx context.Context, entry mediaEntry, dest string) (int64, error)

// buildGalleryArchive downloads every asset a post declares into a ZIP laid
// out exactly like the native gallery-dl artifact — numbered media files,
// Arker's normalized metadata.json, and the sanitized raw provider record — so
// the existing gallery viewer and API serve an Apify bundle with no
// changes.
//
// Completeness is measured against what the provider record said the post
// holds, never against what happened to download. That is the difference
// between "a three-image post" and "three images of a ten-image post", and it
// is the only reason a partial rescue cannot read green.
func (c *Client) buildGalleryArchive(ctx context.Context, entries []mediaEntry, audio *galleryAudio, meta *archivers.GalleryMetadata, record map[string]any, fetch mediaFetcher, logWriter io.Writer) (archivers.Result, archivers.Completeness, int64, error) {
	if len(entries) == 0 {
		return archivers.Result{}, archivers.Completeness{}, 0, fmt.Errorf("Apify record contains no media")
	}

	tmpDir, err := os.MkdirTemp("", "arker-apify-gallery-*")
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
		canonicalName, contentType, err := canonicalizeDownloadedGalleryMedia(tmpDir, name, logWriter)
		if err != nil {
			return archivers.Result{}, archivers.Completeness{}, totalBytes, err
		}
		name = canonicalName
		totalBytes += size
		meta.Files = append(meta.Files, archivers.GalleryFile{
			Name:        name,
			Size:        size,
			ContentType: contentType,
			IsVideo:     strings.HasPrefix(contentType, "video/"),
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

	// Read duration and dimensions off the downloaded bytes while they are
	// still files: provider records rarely carry per-slide intrinsics, and the
	// ZIP about to be built is immutable once stored.
	archivers.ProbeGalleryVideoFiles(ctx, tmpDir, meta.Files, logWriter)

	// The soundtrack is fetched after the slides and outside the count: it is
	// part of the post, but a post is its slides, and a track TikTok will not
	// serve must not make a whole slideshow read as partial.
	totalBytes += fetchGalleryAudio(ctx, tmpDir, audio, meta, fetch, logWriter)

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
		Source:       models.ArchiveSourceApify,
		Completeness: completeness.State,
	}, completeness, totalBytes, nil
}

// canonicalizeDownloadedGalleryMedia makes the provider's numbered internal
// filename describe the bytes that actually arrived from the provider. The
// raw provider record remains raw; metadata.json and ZIP entries use the
// canonical name returned here.
func canonicalizeDownloadedGalleryMedia(dir, name string, logWriter io.Writer) (string, string, error) {
	canonicalName, contentType, err := archivers.InspectGalleryMediaFile(dir, name)
	if err != nil {
		return "", "", fmt.Errorf("inspect downloaded media %s: %w", name, err)
	}
	if canonicalName == name {
		return name, contentType, nil
	}
	if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, canonicalName)); err != nil {
		return "", "", fmt.Errorf("rename downloaded media %s to %s: %w", name, canonicalName, err)
	}
	fmt.Fprintf(logWriter, "Normalized gallery media filename %s to %s based on its bytes\n", name, canonicalName)
	return canonicalName, contentType, nil
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
// be. Only MP4/MOV containers are checkable by format this cheaply (both are
// ISO base media, so "ftyp" at offset 4 identifies them).
//
// Every entry is additionally checked for being an HTML document, because that
// is the one non-media body a CDN hands back with a 200: a login wall, an error
// page, or — on Facebook, where an attachment's `url` is sometimes the post's
// own page rather than its image — the post itself. Stored unchecked it would
// be a .jpg holding a web page, counted towards completeness as if the slide
// had been archived. Image formats themselves are not decoded: a decoder that
// does not know WEBP or HEIC would reject real media, which is a worse failure
// than the one it prevents.
func verifyGalleryMedia(entry mediaEntry, path string) error {
	if err := rejectHTMLDocument(path); err != nil {
		return err
	}
	switch entry.extension() {
	case ".mp4", ".mov", ".m4a":
		return verifyMP4(path)
	}
	return nil
}

// rejectHTMLDocument fails when a downloaded media file is really a web page.
func rejectHTMLDocument(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, 512)
	n, err := f.Read(header)
	if err != nil && n == 0 {
		return fmt.Errorf("downloaded media is empty")
	}
	prefix := strings.ToLower(strings.TrimSpace(string(header[:n])))
	for _, marker := range []string{"<!doctype html", "<html", "<head", "<?xml"} {
		if strings.HasPrefix(prefix, marker) {
			return fmt.Errorf("downloaded media is an HTML document, not the post's media")
		}
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
//
// The last pattern requires a non-alphanumeric boundary on both sides of the
// marker, which is the whole reason it is safe to use. Reddit's media IDs are
// random base36 — "q7f331pd9k2la" contains "331p" — and about one in 240 of
// them reads as a resolution to an unbounded pattern. Matching one would score
// every variant of that video identically (they share the ID), so the
// first-listed, which for Reddit is the smallest, would win silently: a 392p
// copy of a 1080p video, archived with no error and a fabricated quality
// label. Requiring the boundary costs the occasional real marker glued to a
// word ("video1080p.mp4"), which merely means the variants are not collapsed —
// the safe direction to be wrong in.
var videoResolutionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(\d{2,5})x(\d{2,5})$`),  // X: .../vid/avc1/3840x2160/name.mp4
	regexp.MustCompile(`(?i)res[_-]?(\d{2,5})p`), // Reddit: .../pb/m2-res_1080p.mp4
	regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(\d{3,4})p(?:$|[^a-z0-9])`),
}

// resolutionMatch locates a resolution marker within a URL's path.
type resolutionMatch struct {
	// segment is the index of the path segment carrying the marker; everything
	// before it identifies the video the variants belong to.
	segment int
	score   int64
	label   string
}

// findVideoResolution scans a path for a resolution marker, most specific
// pattern first rather than left-most segment first.
//
// Pattern confidence has to outrank position: a path can carry a weak marker
// early and the real one late, and taking the left-most match would group on
// the weak one. Scores from different patterns are never compared — a match
// fixes both the group and the scale, and every variant of one video uses the
// same URL scheme.
func findVideoResolution(segments []string) (resolutionMatch, bool) {
	for _, pattern := range videoResolutionPatterns {
		for i, segment := range segments {
			m := pattern.FindStringSubmatch(segment)
			if m == nil {
				continue
			}
			if len(m) > 2 && m[2] != "" {
				width, err := strconv.ParseInt(m[1], 10, 64)
				height, hErr := strconv.ParseInt(m[2], 10, 64)
				if err != nil || hErr != nil {
					continue
				}
				return resolutionMatch{segment: i, score: width * height, label: m[0]}, true
			}
			value, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				continue
			}
			return resolutionMatch{segment: i, score: value, label: m[1] + "p"}, true
		}
	}
	return resolutionMatch{}, false
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
	match, ok := findVideoResolution(segments)
	if !ok {
		return rawURL, 0
	}
	return parsed.Host + strings.Join(segments[:match.segment], "/"), match.score
}

// videoQualityLabel names the resolution a media URL advertises, for the
// normalized metadata's quality_label. Empty when the URL says nothing, which
// is more useful than a number invented from an ID.
func videoQualityLabel(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if match, ok := findVideoResolution(strings.Split(parsed.Path, "/")); ok {
		return match.label
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

// attachSingleVideoMetadata describes a gallery archive that is exactly one
// video with the normalized video contract as well, so a Reddit, X, Facebook
// or Pinterest video post archived through the gallery flow carries the same
// machine-readable facts (duration, engagement, publish time) as a yt-dlp
// capture. meta is the video description, complete except for Media, which
// is filled from the stored file.
func attachSingleVideoMetadata(result *archivers.Result, galleryMeta *archivers.GalleryMetadata, record map[string]any, meta *archivers.VideoMetadata, product string) error {
	var stored *archivers.GalleryFile
	for i := range galleryMeta.Files {
		if galleryMeta.Files[i].IsVideo {
			stored = &galleryMeta.Files[i]
			break
		}
	}
	if stored == nil {
		return nil
	}
	media := meta.Media
	media.Extension = path.Ext(stored.Name)
	media.ContentType = stored.ContentType
	media.SizeBytes = stored.Size
	if stored.Width > 0 && stored.Height > 0 {
		w, h := int64(stored.Width), int64(stored.Height)
		media.Width, media.Height = &w, &h
	}
	if meta.DurationSeconds == nil && stored.DurationSeconds != nil {
		meta.DurationSeconds = stored.DurationSeconds
	}
	metadata, raw, err := metadataOnlyVideoArtifacts(meta, record, media, product)
	if err != nil {
		return err
	}
	result.Metadata, result.RawMetadata = metadata, raw
	return nil
}
