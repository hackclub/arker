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
	if item == nil || item.Status != "completed" || item.StorageKey == "" {
		return nil, nil, false
	}
	size, err := storageInstance.Size(item.StorageKey)
	if err != nil {
		return nil, nil, false
	}
	if seekable, ok := storageInstance.(storage.SeekableStorage); ok {
		reader, err := seekable.SeekableReader(item.StorageKey)
		if err == nil {
			zr, err := zip.NewReader(&seekerReaderAt{seeker: reader}, size)
			if err == nil {
				return zr, func() { _ = reader.Close() }, true
			}
			_ = reader.Close()
		}
	}
	if size > maxGalleryBufferedSize {
		return nil, nil, false
	}
	r, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		return nil, nil, false
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, false
	}
	zr, err := zip.NewReader(&bytesReaderAtCloser{data: data}, int64(len(data)))
	if err != nil {
		return nil, nil, false
	}
	return zr, func() {}, true
}

// ServeGalleryManifest returns the normalized post metadata plus the list of
// media files in the archive. The viewer calls this to render a post.
func ServeGalleryManifest(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
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
	if redirectIfAlias(c, db, shortID) {
		return
	}
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").Where("captures.short_id = ? AND archive_items.type = ?", shortID, utils.ArchiveTypeGalleryDl).First(&item).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "gallery archive not found"})
		return
	}
	zr, cleanup, ok := openGalleryZipData(storageInstance, &item)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "raw gallery metadata not available"})
		return
	}
	defer cleanup()
	type record struct {
		Filename string          `json:"filename"`
		Metadata json.RawMessage `json:"metadata"`
	}
	records := make([]record, 0)
	for _, f := range zr.File {
		if f.Name == galleryMetadataFilename || !strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			continue
		}
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
		records = append(records, record{Filename: f.Name, Metadata: safe})
	}
	if len(records) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "raw gallery metadata not available"})
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, gin.H{"provider": func() string {
		for _, r := range records {
			if r.Filename == "brightdata.json" {
				return "brightdata"
			}
		}
		return "gallery-dl"
	}(), "records": records})
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
