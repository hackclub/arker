package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// galleryMetadataFilename mirrors the name the archiver writes at the ZIP root.
const galleryMetadataFilename = "metadata.json"

// maxGalleryBufferedSize caps how large a gallery ZIP may be before it is
// refused when the storage backend cannot seek. Seekable backends (S3 ranged
// GETs, local files) stream and are not subject to this.
const maxGalleryBufferedSize = 200 * 1024 * 1024

// maxGalleryEntrySize caps a single media file served inline. One entry is one
// image or one video from a post, so this is generous; anything larger is only
// available through the full-archive download, which streams.
const maxGalleryEntrySize = 256 * 1024 * 1024

// seekerReaderAt adapts a ReadSeekCloser to io.ReaderAt for archive/zip. The
// mutex is what makes it safe: ReaderAt is documented as safe for concurrent
// use, but a seek-based implementation shares one cursor.
type seekerReaderAt struct {
	mu     sync.Mutex
	seeker storage.ReadSeekCloser
}

func (r *seekerReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.seeker.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.seeker, p)
	// io.ReaderAt's contract is to report a short read at end-of-input as
	// io.EOF; io.ReadFull reports it as io.ErrUnexpectedEOF, which callers
	// like archive/zip treat as a hard failure rather than end-of-file.
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

// bytesReaderAtCloser adapts an in-memory buffer to the same interface.
type bytesReaderAtCloser struct{ data []byte }

func (r *bytesReaderAtCloser) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// openGalleryZip locates a capture's gallery-dl archive and opens it for
// random access, preferring ranged reads over buffering the whole ZIP.
func openGalleryZip(c *gin.Context, storageInstance storage.Storage, db *gorm.DB, shortID string) (*zip.Reader, func(), bool) {
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, utils.ArchiveTypeGalleryDl).
		First(&item).Error; err != nil {
		c.Status(http.StatusNotFound)
		return nil, nil, false
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return nil, nil, false
	}

	size, err := storageInstance.Size(item.StorageKey)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Archive temporarily unavailable"})
		return nil, nil, false
	}

	if seekable, ok := storageInstance.(storage.SeekableStorage); ok {
		reader, err := seekable.SeekableReader(item.StorageKey)
		if err == nil {
			zipReader, err := zip.NewReader(&seekerReaderAt{seeker: reader}, size)
			if err != nil {
				reader.Close()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive is not a readable ZIP"})
				return nil, nil, false
			}
			return zipReader, func() { reader.Close() }, true
		}
		// Fall through to buffering when the backend cannot seek this object.
	}

	if size > maxGalleryBufferedSize {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Archive too large to browse; download the full archive instead",
		})
		return nil, nil, false
	}

	reader, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Archive temporarily unavailable"})
		return nil, nil, false
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to read archive"})
		return nil, nil, false
	}

	zipReader, err := zip.NewReader(&bytesReaderAtCloser{data: data}, int64(len(data)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive is not a readable ZIP"})
		return nil, nil, false
	}
	return zipReader, func() {}, true
}

// openGalleryZipData opens a known completed gallery item without writing an
// HTTP response. It is shared by the unified result and raw-metadata adapters.
func openGalleryZipData(storageInstance storage.Storage, item *models.ArchiveItem) (*zip.Reader, func(), bool) {
	zr, cleanup, err := openGalleryZipItem(storageInstance, item)
	if err != nil {
		return nil, nil, false
	}
	return zr, cleanup, true
}

// openGalleryZipItem is openGalleryZipData with the failure reason kept, so a
// caller can tell "this bundle cannot be read right now" (retryable) from "this
// item holds no bundle" (permanent).
func openGalleryZipItem(storageInstance storage.Storage, item *models.ArchiveItem) (*zip.Reader, func(), error) {
	if item == nil || item.Status != "completed" || item.StorageKey == "" {
		return nil, nil, fmt.Errorf("gallery item has no stored bundle")
	}
	size, err := storageInstance.Size(item.StorageKey)
	if err != nil {
		return nil, nil, fmt.Errorf("stat gallery bundle: %w", err)
	}
	if seekable, ok := storageInstance.(storage.SeekableStorage); ok {
		reader, err := seekable.SeekableReader(item.StorageKey)
		if err == nil {
			zr, err := zip.NewReader(&seekerReaderAt{seeker: reader}, size)
			if err == nil {
				return zr, func() { _ = reader.Close() }, nil
			}
			_ = reader.Close()
		}
	}
	if size > maxGalleryBufferedSize {
		return nil, nil, fmt.Errorf("gallery bundle exceeds %d bytes and the backend cannot seek", maxGalleryBufferedSize)
	}
	return openGalleryZipItemBuffered(storageInstance, item, size)
}

// openGalleryZipItemBuffered downloads a bounded bundle once and serves all
// subsequent ZIP reads from memory. This is much faster than one ranged object
// request per entry for unusually large carousels/feed captures whose raw
// endpoint must inspect hundreds of tiny JSON sidecars.
func openGalleryZipItemBuffered(storageInstance storage.Storage, item *models.ArchiveItem, size int64) (*zip.Reader, func(), error) {
	if size < 0 || size > maxGalleryBufferedSize {
		return nil, nil, fmt.Errorf("gallery bundle exceeds %d-byte buffer limit", maxGalleryBufferedSize)
	}
	r, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		return nil, nil, fmt.Errorf("open gallery bundle: %w", err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxGalleryBufferedSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read gallery bundle: %w", err)
	}
	if int64(len(data)) > maxGalleryBufferedSize {
		return nil, nil, fmt.Errorf("gallery bundle exceeds %d-byte buffer limit", maxGalleryBufferedSize)
	}
	zr, err := zip.NewReader(&bytesReaderAtCloser{data: data}, int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("gallery bundle is not a readable ZIP: %w", err)
	}
	return zr, func() {}, nil
}

// ServeGalleryList returns the normalized post metadata plus the list of
// media files in the archive. The viewer calls this to render a post.
//
// API consumers should prefer ServeGalleryManifest (/gallery/:shortid/manifest),
// which carries capture status, explicit slide order and absolute media URLs.
// This endpoint predates it and its shape is frozen for the viewer.
func ServeGalleryList(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")

	zipReader, cleanup, ok := openGalleryZip(c, storageInstance, db, shortID)
	if !ok {
		return
	}
	defer cleanup()

	manifest := gin.H{"short_id": shortID}

	for _, file := range zipReader.File {
		if file.Name != galleryMetadataFilename {
			continue
		}
		contents, err := file.Open()
		if err != nil {
			break
		}
		raw, err := io.ReadAll(io.LimitReader(contents, 4*1024*1024))
		contents.Close()
		if err != nil {
			break
		}
		var metadata map[string]interface{}
		if err := json.Unmarshal(raw, &metadata); err == nil {
			manifest["metadata"] = metadata
		}
		break
	}

	// Derive the file list from the ZIP itself rather than trusting
	// metadata.json, so the viewer can never link to an entry that is not there.
	files := make([]gin.H, 0, len(zipReader.File))
	for _, file := range zipReader.File {
		if file.Name == galleryMetadataFilename || strings.HasSuffix(file.Name, ".json") {
			continue
		}
		if archivers.GalleryAudioFilename(file.Name) {
			// The soundtrack is not a slide. It is surfaced separately so the
			// viewer plays it under the post instead of as a broken image.
			manifest["audio"] = gin.H{
				"name":         file.Name,
				"size":         file.UncompressedSize64,
				"content_type": galleryZipFileContentType(file),
				"url":          fmt.Sprintf("/gallery/%s/file/%s", shortID, url.PathEscape(file.Name)),
			}
			continue
		}
		files = append(files, gin.H{
			"name":         file.Name,
			"size":         file.UncompressedSize64,
			"content_type": galleryZipFileContentType(file),
			"url":          fmt.Sprintf("/gallery/%s/file/%s", shortID, url.PathEscape(file.Name)),
		})
	}
	manifest["files"] = files

	c.JSON(http.StatusOK, manifest)
}

// ServeGalleryRawMetadata exposes sanitized provider sidecars from the ZIP.
// It never returns media or Arker's normalized metadata.json.
func ServeGalleryRawMetadata(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	item, ok := findGalleryItem(c, db, shortID)
	if !ok {
		return
	}
	response := gin.H{
		"short_id":       shortID,
		"capture_status": item.Status,
		"provider":       galleryProvider(item.Source),
		"records":        []galleryRawRecord{},
	}
	// The raw endpoint is also the kind discriminator. A known gallery must
	// therefore answer 200 while pending or after failure, just as the video
	// manifest does. Absence of an item, not absence of bytes, is the 404.
	if item.Status != "completed" || item.StorageKey == "" {
		response["metadata_unavailable_reason"] = "capture_not_completed"
		c.JSON(http.StatusOK, response)
		return
	}
	zr, cleanup, ok := openGalleryZipData(storageInstance, &item)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "raw gallery metadata temporarily unavailable"})
		return
	}
	defer cleanup()

	mediaEntries, rawEntries, metadataEntry := galleryRawEntries(zr)
	// A seekable S3 ZIP is ideal for ordinary posts because raw metadata does
	// not require downloading the media. At high entry counts, though, hundreds
	// of tiny ranged GETs cost minutes. A bounded one-time read is dramatically
	// cheaper and makes the discriminator endpoint reliably answer.
	if len(rawEntries) > 64 {
		if size, err := storageInstance.Size(item.StorageKey); err == nil && size <= maxGalleryBufferedSize {
			cleanup()
			zr, cleanup, err = openGalleryZipItemBuffered(storageInstance, &item, size)
			if err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "raw gallery metadata temporarily unavailable"})
				return
			}
			defer cleanup()
			mediaEntries, rawEntries, metadataEntry = galleryRawEntries(zr)
		}
	}

	var normalized archivers.GalleryMetadata
	if metadataEntry != nil {
		raw, err := readGalleryZipEntry(metadataEntry, maxGalleryManifestMetadataSize)
		if err == nil {
			_ = json.Unmarshal(raw, &normalized)
		}
	}
	sortGalleryMediaEntries(mediaEntries)
	mediaURLs := make([]string, len(mediaEntries))
	mediaByName := make(map[string]string, len(mediaEntries))
	photoCount := 0
	for i, media := range mediaEntries {
		mediaURLs[i] = fullPath(c, fmt.Sprintf("gallery/%s/file/%s", shortID, url.PathEscape(media.Name)))
		mediaByName[media.Name] = mediaURLs[i]
		// Capture-time canonicalization makes stored gallery extensions honest;
		// avoid opening every media entry merely to count photos here.
		if strings.HasPrefix(archivers.GalleryMediaContentType(media.Name, nil), "image/") {
			photoCount++
		}
	}

	records := make([]galleryRawRecord, 0, max(len(rawEntries), len(mediaEntries)))
	for index, f := range rawEntries {
		r, err := f.Open()
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(r, 4*1024*1024+1))
		r.Close()
		if err != nil || len(raw) > 4*1024*1024 {
			continue
		}
		safe, err := archivers.SanitizeJSON(raw, utils.MediaProxyRedactionSecrets())
		if err != nil {
			continue
		}
		metadata := map[string]interface{}{}
		if err := json.Unmarshal(safe, &metadata); err != nil {
			continue
		}
		normalizeGalleryRawFields(metadata, normalized, photoCount)
		urls := galleryRecordMediaURLs(f.Name, index, len(rawEntries), mediaEntries, mediaURLs, mediaByName)
		addGalleryMediaURLs(metadata, urls)
		records = append(records, galleryRawRecord{Filename: f.Name, Metadata: metadata, MediaURL: firstStringOrEmpty(urls), MediaURLs: urls})
	}
	// A legacy bundle can contain the durable media and normalized metadata but
	// no provider sidecar. Emit one honest normalized record per asset instead
	// of turning a valid gallery ID into a 404 or an unusable empty response.
	if len(records) == 0 {
		for i, media := range mediaEntries {
			metadata := map[string]interface{}{}
			normalizeGalleryRawFields(metadata, normalized, photoCount)
			urls := []string{mediaURLs[i]}
			addGalleryMediaURLs(metadata, urls)
			records = append(records, galleryRawRecord{Filename: media.Name, Metadata: metadata, MediaURL: mediaURLs[i], MediaURLs: urls})
		}
	}
	c.Header("X-Content-Type-Options", "nosniff")
	response["provider"] = func() string {
		for _, r := range records {
			if p := fallbackRawRecordProvider(r.Filename); p != "" {
				return p
			}
		}
		return galleryProvider(item.Source)
	}()
	response["records"] = records
	c.JSON(http.StatusOK, response)
}

func galleryRawEntries(zr *zip.Reader) (media, raw []*zip.File, metadata *zip.File) {
	media = make([]*zip.File, 0, len(zr.File))
	raw = make([]*zip.File, 0, len(zr.File))
	for _, f := range zr.File {
		switch {
		case f.Name == galleryMetadataFilename:
			metadata = f
		case strings.HasSuffix(strings.ToLower(f.Name), ".json"):
			raw = append(raw, f)
		case !f.FileInfo().IsDir():
			media = append(media, f)
		}
	}
	return media, raw, metadata
}

type galleryRawRecord struct {
	Filename  string                 `json:"filename"`
	Metadata  map[string]interface{} `json:"metadata"`
	MediaURL  string                 `json:"media_url,omitempty"`
	MediaURLs []string               `json:"media_urls"`
}

func galleryProvider(source string) string {
	if models.IsFallbackSource(source) {
		return source
	}
	return "gallery-dl"
}

// fallbackRawRecordProvider names the paid provider whose raw record a bundle
// entry is, or "" for anything else. Apify bundles carry apify.json; Bright
// Data bundles archived before the swap carry brightdata.json.
func fallbackRawRecordProvider(filename string) string {
	switch filename {
	case "apify.json":
		return models.ArchiveSourceApify
	case "brightdata.json":
		return models.ArchiveSourceBrightData
	}
	return ""
}

func normalizeGalleryRawFields(raw map[string]interface{}, normalized archivers.GalleryMetadata, photoCount int) {
	raw["caption"] = firstGalleryRawValue(raw, []string{"caption", "description", "text", "content", "post_content"}, firstNonBlank(normalized.Description, normalized.Title))
	raw["user_posted"] = firstGalleryRawValue(raw, []string{"user_posted", "username", "author", "uploader", "owner"}, firstNonBlank(normalized.Author, normalized.AuthorName))
	raw["date_posted"] = firstGalleryRawValue(raw, []string{"date_posted", "post_date", "date", "created_at", "taken_at", "timestamp"}, normalized.Date)
	raw["photos_number"] = firstGalleryRawValue(raw, []string{"photos_number", "photo_count", "image_count"}, photoCount)
	raw["likes"] = firstGalleryRawValue(raw, []string{"likes", "like_count", "favorites", "favorite_count"}, normalized.Likes)
	raw["num_comments"] = firstGalleryRawValue(raw, []string{"num_comments", "comments", "comment_count"}, normalized.Comments)
}

func firstGalleryRawValue(raw map[string]interface{}, keys []string, fallback interface{}) interface{} {
	for _, key := range keys {
		if value, exists := raw[key]; exists && value != nil && value != "" {
			return value
		}
	}
	return fallback
}

func firstNonBlank(values ...string) interface{} {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return nil
}

func galleryRecordMediaURLs(sidecar string, index, rawCount int, media []*zip.File, urls []string, byName map[string]string) []string {
	if name := strings.TrimSuffix(sidecar, ".json"); byName[name] != "" {
		return []string{byName[name]}
	}
	if rawCount == len(media) && index < len(urls) {
		return []string{urls[index]}
	}
	if rawCount == 1 {
		return append([]string(nil), urls...)
	}
	return []string{}
}

func addGalleryMediaURLs(metadata map[string]interface{}, urls []string) {
	metadata["media_urls"] = urls
	if len(urls) == 1 {
		metadata["media_url"] = urls[0]
	} else {
		metadata["media_url"] = nil
	}
}

func firstStringOrEmpty(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return ""
}

// ServeGalleryFile serves a single media file out of the gallery-dl ZIP.
func ServeGalleryFile(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")

	requestedPath := c.Param("filepath")
	if decoded, err := url.QueryUnescape(requestedPath); err == nil {
		requestedPath = decoded
	}
	requestedPath = path.Clean(strings.TrimPrefix(requestedPath, "/"))
	// The archiver writes a flat ZIP, so any traversal or nesting is bogus.
	if requestedPath == "" || requestedPath == "." || strings.Contains(requestedPath, "/") {
		c.Status(http.StatusNotFound)
		return
	}

	zipReader, cleanup, ok := openGalleryZip(c, storageInstance, db, shortID)
	if !ok {
		return
	}
	defer cleanup()

	var target *zip.File
	for _, file := range zipReader.File {
		if file.Name == requestedPath {
			target = file
			break
		}
	}
	if target == nil {
		c.Status(http.StatusNotFound)
		return
	}
	serveGalleryZipEntry(c, shortID, target)
}

// serveGalleryZipEntry serves one already-selected gallery entry as ordinary
// media. The video compatibility path uses the same implementation so Range,
// HEAD, MIME sniffing, and size limits cannot drift between URLs.
func serveGalleryZipEntry(c *gin.Context, shortID string, target *zip.File) {
	contents, err := target.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file from archive"})
		return
	}
	defer contents.Close()

	// Buffer the entry so http.ServeContent can answer Range requests. Video
	// slides need this: Safari and iOS open a video with "Range: bytes=0-1" and
	// refuse to play against a 200, and seeking is broken everywhere without it.
	// Entries are single post media, bounded by maxGalleryEntrySize.
	if target.UncompressedSize64 > maxGalleryEntrySize {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "File too large to serve individually; download the full archive instead",
		})
		return
	}
	data, err := io.ReadAll(io.LimitReader(contents, int64(maxGalleryEntrySize)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file from archive"})
		return
	}

	c.Header("Content-Type", archivers.GalleryMediaContentType(target.Name, data))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("ETag", fmt.Sprintf("\"%s-%d-%x\"", shortID, target.UncompressedSize64, target.CRC32))
	// ServeContent sets Content-Length/Content-Range and handles Range and
	// If-None-Match. The zero modtime keeps it from emitting Last-Modified.
	http.ServeContent(c.Writer, c.Request, target.Name, time.Time{}, bytes.NewReader(data))
}

// galleryFileContentType is the extension-only fallback for unreadable ZIP
// entries. Normal serving paths use galleryZipFileContentType or the already
// buffered entry bytes so misleading provider extensions cannot win.
func galleryFileContentType(name string) string {
	return archivers.GalleryMediaContentType(name, nil)
}

func galleryZipFileContentType(file *zip.File) string {
	contents, err := file.Open()
	if err != nil {
		return galleryFileContentType(file.Name)
	}
	defer contents.Close()
	header := make([]byte, 512)
	n, err := contents.Read(header)
	if err != nil && err != io.EOF {
		return galleryFileContentType(file.Name)
	}
	return archivers.GalleryMediaContentType(file.Name, header[:n])
}
