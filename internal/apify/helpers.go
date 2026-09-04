package apify

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

// rawRecordName is the ZIP entry holding the sanitized provider record beside
// metadata.json, mirroring how the native flows keep their extractor output.
const rawRecordName = "apify.json"

// mediaEntry is one media slide resolved from a post record.
type mediaEntry struct {
	URL  string
	Type string // "Photo" | "Video" | "Audio"
}

func (e mediaEntry) isAudio() bool {
	return strings.EqualFold(e.Type, "audio")
}

func (e mediaEntry) isVideo() bool {
	return strings.EqualFold(e.Type, "video") || strings.EqualFold(e.Type, "reel")
}

func (e mediaEntry) extension() string {
	if ext := strings.ToLower(path.Ext(urlPath(e.URL))); ext != "" && len(ext) <= 5 {
		switch ext {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic", ".mp4", ".mov", ".webm", ".mp3", ".m4a", ".aac", ".ogg":
			return ext
		}
	}
	if e.isAudio() {
		return ".mp3"
	}
	if e.isVideo() {
		return ".mp4"
	}
	return ".jpg"
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
	rawJSON, err := sanitizedRecord(record)
	if err != nil {
		return "", err
	}

	zipFile, err := createTempFile("arker-apify-gallery-*.zip")
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
	if err := writeEntry(rawRecordName, zip.Deflate, func(w io.Writer) error {
		_, err := w.Write(rawJSON)
		return err
	}); err != nil {
		zw.Close()
		zipFile.Close()
		return "", err
	}

	mediaNames := make([]string, 0, len(meta.Files)+1)
	for _, file := range meta.Files {
		mediaNames = append(mediaNames, file.Name)
	}
	if meta.Music != nil && meta.Music.File != "" {
		// The soundtrack travels with the slides; it is simply not one of them.
		mediaNames = append(mediaNames, meta.Music.File)
	}
	for _, name := range mediaNames {
		// Media is stored uncompressed, matching the native flow: JPEG/MP4
		// does not deflate meaningfully.
		if err := writeEntry(name, zip.Store, func(w io.Writer) error {
			f, err := os.Open(filepath.Join(dir, name))
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

// sanitizedRecord serializes a provider record with signed media URLs
// redacted, the form every stored raw record takes.
func sanitizedRecord(record map[string]any) ([]byte, error) {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	rawJSON, err := archivers.SanitizeJSON(recordJSON, nil)
	if err != nil {
		return nil, fmt.Errorf("sanitize Apify record: %w", err)
	}
	return rawJSON, nil
}

// rawSidecar wraps the sanitized record as the raw metadata sidecar of a
// video artifact.
func rawSidecar(record map[string]any) (*archivers.Sidecar, error) {
	rawJSON, err := sanitizedRecord(record)
	if err != nil {
		return nil, err
	}
	return &archivers.Sidecar{Data: rawJSON}, nil
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
		return &archivers.Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialPreview}
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
	tmpPath, _, err := c.downloadToTemp(ctx, imageURL, "arker-apify-thumb-*")
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
	return &archivers.Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialPreview}, nil
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

// nested walks a path of object keys and returns the value at the end, or nil.
func nested(record map[string]any, keys ...string) any {
	var current any = record
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = obj[key]
	}
	return current
}

// nestedObject is nested for a final object value.
func nestedObject(record map[string]any, keys ...string) map[string]any {
	obj, _ := nested(record, keys...).(map[string]any)
	return obj
}

// nestedList is nested for a final list value.
func nestedList(record map[string]any, keys ...string) []any {
	list, _ := nested(record, keys...).([]any)
	return list
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

// objectStrings reads one string key out of every object in a list, skipping
// blanks: TikTok's hashtags are [{name: "humor"}, {name: ""}].
func objectStrings(values []any, keys ...string) []string {
	var result []string
	for _, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			if s := stringField(typed, keys...); s != "" {
				result = append(result, s)
			}
		case string:
			if s := strings.TrimSpace(typed); s != "" {
				result = append(result, s)
			}
		}
	}
	return result
}

func boolField(record map[string]any, keys ...string) *bool {
	for _, key := range keys {
		if value, ok := record[key].(bool); ok {
			return &value
		}
	}
	return nil
}

// intField reads a counter from a provider record.
//
// Numbers arrive as strings often enough to matter, and a reader that only
// accepts JSON numbers drops those silently, which shows up as an archive that
// reports no shares rather than as an error. Strings are parsed strictly: a
// value like "1.2K" is not a count this can honestly report.
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
// provider record. Parsing is strict for the same reason intField's is.
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

// positiveFloat returns the value only when it is a usable positive quantity.
func positiveFloat(value *float64) *float64 {
	if value == nil || *value <= 0 {
		return nil
	}
	return value
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// timestampString normalizes the timestamps actors publish — RFC 3339 strings
// or epoch seconds/milliseconds — into RFC 3339 UTC. Unknown shapes are
// returned as-is when they are strings and dropped otherwise. The rule is
// shared with the gallery raw-metadata endpoint, which has to reconcile the
// same shapes coming out of stored bundles, so it lives in utils.
func timestampString(value any) string {
	return utils.NormalizeTimestamp(value)
}

var hashtagPattern = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

// hashtagsFromText recovers the hashtags of a caption for actors that publish
// none as a list. Order of first appearance, deduplicated case-insensitively.
func hashtagsFromText(text string) []string {
	seen := map[string]bool{}
	var tags []string
	for _, match := range hashtagPattern.FindAllStringSubmatch(text, -1) {
		key := strings.ToLower(match[1])
		if seen[key] {
			continue
		}
		seen[key] = true
		tags = append(tags, match[1])
	}
	return tags
}

// recordError reports the error an actor wrote into a dataset item instead of
// a post. Every vetted actor does this rather than failing the run: an item
// carrying an error field (or an explicit success:false) and no post ID.
func recordError(record map[string]any, idKeys ...string) error {
	if stringField(record, idKeys...) != "" {
		return nil
	}
	message := stringField(record, "error", "errorCode", "errorDescription", "message")
	if success, ok := record["success"].(bool); ok && !success && message == "" {
		message = "actor reported success=false"
	}
	if message == "" {
		return nil
	}
	lowered := strings.ToLower(message)
	if strings.Contains(lowered, "not found") || strings.Contains(lowered, "private") ||
		strings.Contains(lowered, "not_found") || strings.Contains(lowered, "unavailable") ||
		strings.Contains(lowered, "failed_to_fetch") || strings.Contains(lowered, "no items") ||
		strings.Contains(lowered, "no_items") || strings.Contains(lowered, "empty or private") {
		return fmt.Errorf("%w: %s", errNotFound, message)
	}
	return fmt.Errorf("actor error record: %s", message)
}

// titleFromText derives a video title the way the native extractors do for
// platforms without a title field: the first non-empty line of the caption,
// else "Video by <author>".
func titleFromText(text, author string) string {
	text = strings.TrimSpace(text)
	if line, _, _ := strings.Cut(text, "\n"); strings.TrimSpace(line) != "" {
		return truncate(strings.TrimSpace(line), 120)
	}
	if author != "" {
		return "Video by " + author
	}
	return ""
}
