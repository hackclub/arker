package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// galleryManifestBody mirrors the public manifest contract. The test decodes
// into its own struct rather than the handler's so a field rename in the
// handler shows up here as a failing assertion instead of passing silently.
type galleryManifestBody struct {
	SchemaVersion string `json:"schema_version"`
	ShortID       string `json:"short_id"`
	CaptureStatus string `json:"capture_status"`
	MediaCount    int    `json:"media_count"`
	Media         []struct {
		Index       int    `json:"index"`
		MediaURL    string `json:"media_url"`
		Filename    string `json:"filename"`
		Type        string `json:"type"`
		ContentType string `json:"content_type"`
		SizeBytes   int64  `json:"size_bytes"`
		Width       *int64 `json:"width"`
		Height      *int64 `json:"height"`
		AltText     string `json:"alt_text"`
	} `json:"media"`
	ThumbnailAvailable         bool            `json:"thumbnail_available"`
	ThumbnailURL               *string         `json:"thumbnail_url"`
	ThumbnailUnavailableReason string          `json:"thumbnail_unavailable_reason"`
	ArchiveURL                 *string         `json:"archive_url"`
	MetadataAvailable          bool            `json:"metadata_available"`
	Metadata                   json.RawMessage `json:"metadata"`
	RawMetadataURL             *string         `json:"raw_metadata_url"`
	Provenance                 string          `json:"provenance"`
	MetadataUnavailableReason  string          `json:"metadata_unavailable_reason"`
}

// setGalleryThumbnail marks a capture's gallery item as carrying a stored
// preview image, exactly as StoreThumbnail does after a capture derives one
// from the post's first card.
func setGalleryThumbnail(t *testing.T, db *gorm.DB, store storage.Storage, shortID, key string, data []byte) {
	t.Helper()
	storeTestObject(t, store, key, data)
	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, utils.ArchiveTypeGalleryDl).
		First(&item).Error; err != nil {
		t.Fatalf("load gallery item: %v", err)
	}
	if err := db.Model(&item).
		Updates(map[string]interface{}{
			"thumbnail_key":    key,
			"thumbnail_width":  480,
			"thumbnail_height": 270,
			"thumbnail_status": models.ThumbnailStatusReady,
		}).Error; err != nil {
		t.Fatalf("set gallery thumbnail: %v", err)
	}
}

// writeGalleryZipFixture stores a ZIP with exactly the given entries under the
// capture's gallery item, replacing whatever seedGalleryCapture wrote.
func writeGalleryZipFixture(t *testing.T, db *gorm.DB, store storage.Storage, shortID string, entries []struct{ name, body string }) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatalf("create %s: %v", entry.name, err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write %s: %v", entry.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, utils.ArchiveTypeGalleryDl).
		First(&item).Error; err != nil {
		t.Fatalf("load gallery item: %v", err)
	}
	writer, err := store.Writer(item.StorageKey)
	if err != nil {
		t.Fatalf("storage writer: %v", err)
	}
	if _, err := writer.Write(buf.Bytes()); err != nil {
		t.Fatalf("storage write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("storage close: %v", err)
	}
}

// getGalleryManifest drives the real router so route registration, the alias
// hop, and the absolute URL builder are all exercised.
func getGalleryManifest(t *testing.T, router *gin.Engine, shortID string) (*httptest.ResponseRecorder, galleryManifestBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/gallery/"+shortID+"/manifest", nil)
	req.Host = "archive.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var body galleryManifestBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode manifest: %v (body %s)", err, rec.Body.String())
		}
	}
	return rec, body
}

// A consumer must be able to go from a short ID to every card's bytes without
// downloading the ZIP and without knowing which tool captured the post.
func TestServeGalleryManifestReportsPerCardMediaURLs(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "7fbf9", "completed")
	writeGalleryZipFixture(t, db, store, "7fbf9", []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","extractor":"instagram","author":"someone","author_name":"Some One","description":"a caption","file_count":2,"files":[` +
			`{"name":"001.jpg","size":10,"content_type":"image/jpeg","is_video":false,"width":1080,"height":1350,"alt_text":"a friend soldering"},` +
			`{"name":"002.mp4","size":9,"content_type":"video/mp4","is_video":true,"width":720,"height":1280}]}`},
		{"001.jpg", "jpeg-bytes"},
		{"001.jpg.json", `{"width":1080,"height":1350}`},
		{"002.mp4", "mp4-bytes"},
		{"002.mp4.json", `{"width":720,"height":1280}`},
	})
	router := newGalleryRouter(db, store)

	rec, manifest := getGalleryManifest(t, router, "7fbf9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if manifest.SchemaVersion == "" || manifest.ShortID != "7fbf9" || manifest.CaptureStatus != "completed" {
		t.Errorf("envelope = %+v, want schema version, short ID and completed status", manifest)
	}
	if !manifest.MetadataAvailable || manifest.MetadataUnavailableReason != "" {
		t.Errorf("metadata_available = %v, reason = %q, want available with no reason", manifest.MetadataAvailable, manifest.MetadataUnavailableReason)
	}
	var normalized map[string]interface{}
	if err := json.Unmarshal(manifest.Metadata, &normalized); err != nil {
		t.Fatalf("decode normalized metadata: %v", err)
	}
	if normalized["author"] != "someone" || normalized["description"] != "a caption" {
		t.Errorf("normalized metadata = %v, want the archived post fields", normalized)
	}

	if manifest.MediaCount != 2 || len(manifest.Media) != 2 {
		t.Fatalf("media = %+v (count %d), want exactly the 2 media files", manifest.Media, manifest.MediaCount)
	}
	first, second := manifest.Media[0], manifest.Media[1]
	if first.Index != 0 || second.Index != 1 {
		t.Errorf("indices = %d, %d, want 0 and 1 in swipe order", first.Index, second.Index)
	}
	if first.Filename != "001.jpg" || first.Type != "image" || first.ContentType != "image/jpeg" {
		t.Errorf("media[0] = %+v, want the first still", first)
	}
	if second.Filename != "002.mp4" || second.Type != "video" || second.ContentType != "video/mp4" {
		t.Errorf("media[1] = %+v, want the video slide", second)
	}
	if first.SizeBytes != int64(len("jpeg-bytes")) {
		t.Errorf("media[0].size_bytes = %d, want %d", first.SizeBytes, len("jpeg-bytes"))
	}
	if first.Width == nil || *first.Width != 1080 || first.Height == nil || *first.Height != 1350 {
		t.Errorf("media[0] dimensions = %v x %v, want 1080x1350 from the stored metadata", first.Width, first.Height)
	}
	if first.AltText != "a friend soldering" {
		t.Errorf("media[0].alt_text = %q, want the poster's own description", first.AltText)
	}

	// The URL must be complete and must not require the caller to know the
	// capture tool: no consumer should ever have to type "gallery-dl".
	if first.MediaURL != "https://archive.test/gallery/7fbf9/file/001.jpg" {
		t.Errorf("media[0].media_url = %q, want an absolute URL to the card", first.MediaURL)
	}
	if manifest.ArchiveURL == nil || *manifest.ArchiveURL != "https://archive.test/archive/7fbf9/gallery-dl" {
		t.Errorf("archive_url = %v, want the absolute ZIP URL", manifest.ArchiveURL)
	}
	if manifest.RawMetadataURL == nil || *manifest.RawMetadataURL != "https://archive.test/gallery/7fbf9/raw" {
		t.Errorf("raw_metadata_url = %v, want the absolute raw URL", manifest.RawMetadataURL)
	}

	// And the reported URL has to actually serve the bytes.
	parsed, err := url.Parse(first.MediaURL)
	if err != nil {
		t.Fatalf("parse media URL: %v", err)
	}
	fileRec := httptest.NewRecorder()
	router.ServeHTTP(fileRec, httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	if fileRec.Code != http.StatusOK || fileRec.Body.String() != "jpeg-bytes" {
		t.Errorf("GET %s = %d, body %q, want the archived card bytes", parsed.Path, fileRec.Code, fileRec.Body.String())
	}
}

// Media is listed in swipe order regardless of the order entries happen to sit
// in the ZIP, and a 10+ card carousel must not sort 010 before 002.
func TestServeGalleryManifestOrdersMediaBySlideNumber(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "ord01", "completed")
	writeGalleryZipFixture(t, db, store, "ord01", []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ORD/","file_count":3}`},
		{"010.jpg", "ten"},
		{"002.jpg", "two"},
		{"001.jpg", "one"},
	})

	_, manifest := getGalleryManifest(t, newGalleryRouter(db, store), "ord01")
	want := []string{"001.jpg", "002.jpg", "010.jpg"}
	if len(manifest.Media) != len(want) {
		t.Fatalf("media = %+v, want %d cards", manifest.Media, len(want))
	}
	for i, name := range want {
		if manifest.Media[i].Filename != name || manifest.Media[i].Index != i {
			t.Errorf("media[%d] = %+v, want %s at index %d", i, manifest.Media[i], name, i)
		}
	}
}

// A capture still running answers with its status rather than 404, so a poller
// can tell "not finished yet" from "no such archive".
func TestServeGalleryManifestReportsCaptureInProgress(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "pend1", "pending")

	rec, manifest := getGalleryManifest(t, newGalleryRouter(db, store), "pend1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manifest.CaptureStatus != "pending" || manifest.MetadataAvailable {
		t.Errorf("manifest = %+v, want pending status with no metadata", manifest)
	}
	if manifest.MetadataUnavailableReason != "capture_not_completed" {
		t.Errorf("reason = %q, want capture_not_completed", manifest.MetadataUnavailableReason)
	}
	if manifest.ArchiveURL != nil {
		t.Errorf("archive_url = %v, want nil while the capture is unfinished", *manifest.ArchiveURL)
	}
	// An empty list, never null: consumers iterate it unconditionally.
	if manifest.Media == nil || len(manifest.Media) != 0 || manifest.MediaCount != 0 {
		t.Errorf("media = %+v (count %d), want an empty list", manifest.Media, manifest.MediaCount)
	}
	if string(manifest.Metadata) != "null" {
		t.Errorf("metadata = %s, want null", manifest.Metadata)
	}
}

func TestServeGalleryManifestReportsFailedCapture(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "fail1", "failed")

	rec, manifest := getGalleryManifest(t, newGalleryRouter(db, store), "fail1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manifest.CaptureStatus != "failed" || manifest.MediaCount != 0 || manifest.MetadataAvailable {
		t.Errorf("manifest = %+v, want a failed capture with no media", manifest)
	}
}

// A bundle written before Arker normalized gallery metadata still has to yield
// fetchable cards; only the metadata block is absent.
func TestServeGalleryManifestServesLegacyArchiveWithoutMetadata(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "leg01", "completed")
	writeGalleryZipFixture(t, db, store, "leg01", []struct{ name, body string }{
		{"001.jpg", "jpeg-bytes"},
	})

	rec, manifest := getGalleryManifest(t, newGalleryRouter(db, store), "leg01")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manifest.MetadataAvailable || string(manifest.Metadata) != "null" {
		t.Errorf("manifest = %+v, want no synthesized metadata", manifest)
	}
	if manifest.MetadataUnavailableReason != "legacy_archive_without_structured_metadata" {
		t.Errorf("reason = %q, want the legacy reason", manifest.MetadataUnavailableReason)
	}
	if manifest.MediaCount != 1 || len(manifest.Media) != 1 || manifest.Media[0].Filename != "001.jpg" {
		t.Fatalf("media = %+v, want the one card the bundle holds", manifest.Media)
	}
	if manifest.Media[0].MediaURL != "https://archive.test/gallery/leg01/file/001.jpg" {
		t.Errorf("media[0].media_url = %q, want the absolute card URL", manifest.Media[0].MediaURL)
	}
	// No provider sidecars in this bundle, so nothing to link.
	if manifest.RawMetadataURL != nil {
		t.Errorf("raw_metadata_url = %v, want nil when the bundle holds no sidecars", *manifest.RawMetadataURL)
	}
}

func TestServeGalleryManifestRejectsUnknownCapture(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "7fbf9", "completed")

	rec, _ := getGalleryManifest(t, newGalleryRouter(db, store), "nope1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a capture with no gallery item", rec.Code)
	}
}

// A carousel's cover is its first card, and the consumer must reach it the
// same way it reaches a video's thumbnail: one URL, read not built.
func TestServeGalleryManifestReportsStoredThumbnail(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "gth01", "completed")
	writeGalleryZipFixture(t, db, store, "gth01", []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","file_count":1,"files":[{"name":"001.jpg","size":10,"content_type":"image/jpeg","is_video":false}]}`},
		{"001.jpg", "jpeg-bytes"},
	})

	thumbBytes := []byte("\xff\xd8\xff\xe0 fake jpeg bytes")
	setGalleryThumbnail(t, db, store, "gth01", "gth01/gallery-dl-abcd-thumb.jpg", thumbBytes)

	router := newGalleryRouter(db, store)
	router.GET("/thumb/:shortid", func(c *gin.Context) { ServeThumbnail(c, store, db, nil) })

	rec, manifest := getGalleryManifest(t, router, "gth01")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !manifest.ThumbnailAvailable {
		t.Errorf("thumbnail_available = false for a gallery holding a stored thumbnail")
	}
	if manifest.ThumbnailUnavailableReason != "" {
		t.Errorf("thumbnail_unavailable_reason = %q on an available thumbnail", manifest.ThumbnailUnavailableReason)
	}
	// Absolute, matching every other URL this manifest reports, and free of
	// the capture tool's name in any segment a caller would have to build.
	if manifest.ThumbnailURL == nil || *manifest.ThumbnailURL != "https://archive.test/thumb/gth01" {
		t.Fatalf("thumbnail_url = %v, want the absolute preview URL", manifest.ThumbnailURL)
	}

	parsed, err := url.Parse(*manifest.ThumbnailURL)
	if err != nil {
		t.Fatalf("parse thumbnail_url: %v", err)
	}
	thumbRec := httptest.NewRecorder()
	router.ServeHTTP(thumbRec, httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	if thumbRec.Code != http.StatusOK {
		t.Fatalf("advertised thumbnail_url returned %d", thumbRec.Code)
	}
	if got := thumbRec.Body.Bytes(); string(got) != string(thumbBytes) {
		t.Errorf("advertised thumbnail_url served %q, want the stored bytes %q", got, thumbBytes)
	}
}

// An all-video post has no still to cover it, and a bundle captured before
// Arker stored previews has none either. Both must read as an explicit no.
func TestServeGalleryManifestSignalsAbsentThumbnail(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "gth02", "completed")
	writeGalleryZipFixture(t, db, store, "gth02", []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/DEF456/","file_count":1,"files":[{"name":"001.mp4","size":9,"content_type":"video/mp4","is_video":true}]}`},
		{"001.mp4", "mp4-bytes"},
	})

	router := newGalleryRouter(db, store)
	rec, manifest := getGalleryManifest(t, router, "gth02")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manifest.ThumbnailAvailable || manifest.ThumbnailURL != nil {
		t.Errorf("manifest advertised a thumbnail it does not have: %+v", manifest)
	}
	if manifest.ThumbnailUnavailableReason != "no_thumbnail_captured" {
		t.Errorf("thumbnail_unavailable_reason = %q, want no_thumbnail_captured", manifest.ThumbnailUnavailableReason)
	}

	// Present-and-false, not omitted: absence must be distinguishable from an
	// older Arker that never carried the field.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"thumbnail_available", "thumbnail_url"} {
		if _, present := raw[field]; !present {
			t.Errorf("%q is omitted; a consumer cannot distinguish no-thumbnail from old-schema", field)
		}
	}
}

// A capture still running has not failed to produce a cover, and must say so
// with the same reason the video manifest uses.
func TestServeGalleryManifestThumbnailReasonForPendingCapture(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "gth03", "pending")

	router := newGalleryRouter(db, store)
	rec, manifest := getGalleryManifest(t, router, "gth03")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manifest.ThumbnailAvailable || manifest.ThumbnailURL != nil {
		t.Errorf("pending capture advertised a thumbnail: %+v", manifest)
	}
	if manifest.ThumbnailUnavailableReason != "capture_not_completed" {
		t.Errorf("thumbnail_unavailable_reason = %q, want capture_not_completed", manifest.ThumbnailUnavailableReason)
	}
}
