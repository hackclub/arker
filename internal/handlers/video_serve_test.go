package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
)

func storeTestObject(t *testing.T, store storage.Storage, key string, data []byte) {
	t.Helper()
	w, err := store.Writer(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeVideoManifestReturnsNormalizedMetadataAndMediaURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "vid01", "https://www.youtube.com/watch?v=fixture", map[string]string{"yt-dlp": "completed"})

	metadataKey := "vid01/yt-dlp-fixture.metadata.json"
	rawKey := "vid01/yt-dlp-fixture.raw-metadata.json"
	metadata := []byte(`{"schema_version":"1","source_url":"https://www.youtube.com/watch?v=fixture","title":"Fixture title","provenance":"native","engagement":{},"media":{"extension":".mp4","content_type":"video/mp4","size_bytes":8},"archived_at":"2026-08-11T22:00:00Z"}`)
	raw := []byte(`{"title":"Fixture title","cookie":"[REDACTED]"}`)
	storeTestObject(t, store, metadataKey, metadata)
	storeTestObject(t, store, rawKey, raw)
	if err := db.Model(&models.ArchiveItem{}).
		Where("type = ?", "yt-dlp").
		Updates(map[string]interface{}{
			"storage_key":      "vid01/yt-dlp-fixture.mp4",
			"metadata_key":     metadataKey,
			"raw_metadata_key": rawKey,
			"source":           models.ArchiveSourceNative,
		}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/video/:shortid/raw", func(c *gin.Context) { ServeVideoRawMetadata(c, store, db) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/vid01/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var manifest struct {
		CaptureStatus     string          `json:"capture_status"`
		MediaURL          *string         `json:"media_url"`
		MetadataAvailable bool            `json:"metadata_available"`
		Metadata          json.RawMessage `json:"metadata"`
		RawMetadataURL    *string         `json:"raw_metadata_url"`
		Provenance        string          `json:"provenance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CaptureStatus != "completed" || !manifest.MetadataAvailable || manifest.MediaURL == nil || *manifest.MediaURL != "/archive/vid01/yt-dlp" {
		t.Errorf("unexpected manifest envelope: %+v", manifest)
	}
	if manifest.RawMetadataURL == nil || *manifest.RawMetadataURL != "/video/vid01/raw" || manifest.Provenance != "native" {
		t.Errorf("unexpected manifest provenance/raw URL: %+v", manifest)
	}
	var normalized map[string]interface{}
	if err := json.Unmarshal(manifest.Metadata, &normalized); err != nil || normalized["title"] != "Fixture title" {
		t.Errorf("normalized metadata = %s, err %v", manifest.Metadata, err)
	}

	rawRec := httptest.NewRecorder()
	router.ServeHTTP(rawRec, httptest.NewRequest(http.MethodGet, "/video/vid01/raw", nil))
	if rawRec.Code != http.StatusOK || rawRec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("raw status = %d type = %q body = %s", rawRec.Code, rawRec.Header().Get("Content-Type"), rawRec.Body.String())
	}
	data, err := io.ReadAll(rawRec.Body)
	if err != nil || string(data) != string(raw) {
		t.Errorf("raw body = %q, err %v", data, err)
	}
}

// thumbnailManifest is the slice of the manifest envelope describing the
// archived preview image. Declared once so every thumbnail test reads the same
// contract a consumer would.
type thumbnailManifest struct {
	CaptureStatus string  `json:"capture_status"`
	ThumbnailURL  *string `json:"thumbnail_url"`
}

func decodeThumbnailManifest(t *testing.T, body []byte) thumbnailManifest {
	t.Helper()
	var manifest thumbnailManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v (body %s)", err, body)
	}
	return manifest
}

// TestServeVideoManifestReportsStoredThumbnail is the consumer's whole reason
// for the field: read a URL out of the manifest, fetch it, get the bytes Arker
// stored. It drives the real /thumb route rather than asserting on the string
// alone, because a URL the manifest advertises but the server does not serve
// would pass a string comparison and fail every caller.
func TestServeVideoManifestReportsStoredThumbnail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm01", "https://www.youtube.com/watch?v=thumb", map[string]string{"yt-dlp": "completed"})

	thumbKey := "thm01/yt-dlp-abcd-thumb.jpg"
	thumbBytes := []byte("\xff\xd8\xff\xe0 fake jpeg bytes")
	storeTestObject(t, store, thumbKey, thumbBytes)
	if err := db.Model(&models.ArchiveItem{}).
		Where("type = ?", "yt-dlp").
		Updates(map[string]interface{}{
			"storage_key":      "thm01/yt-dlp-abcd.mp4",
			"thumbnail_key":    thumbKey,
			"thumbnail_width":  480,
			"thumbnail_height": 270,
			"thumbnail_status": models.ThumbnailStatusReady,
		}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/thumb/:shortid", func(c *gin.Context) { ServeThumbnail(c, store, db, nil) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm01/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", rec.Code, rec.Body.String())
	}
	manifest := decodeThumbnailManifest(t, rec.Body.Bytes())
	if manifest.ThumbnailURL == nil || *manifest.ThumbnailURL != "/thumb/thm01" {
		t.Fatalf("thumbnail_url = %v, want /thumb/thm01", manifest.ThumbnailURL)
	}

	// Follow the advertised URL exactly as a consumer would.
	thumbRec := httptest.NewRecorder()
	router.ServeHTTP(thumbRec, httptest.NewRequest(http.MethodGet, *manifest.ThumbnailURL, nil))
	if thumbRec.Code != http.StatusOK {
		t.Fatalf("advertised thumbnail_url returned %d", thumbRec.Code)
	}
	if got := thumbRec.Body.Bytes(); string(got) != string(thumbBytes) {
		t.Errorf("advertised thumbnail_url served %q, want the stored bytes %q", got, thumbBytes)
	}
	if ct := thumbRec.Header().Get("Content-Type"); ct != thumbnail.ContentType {
		t.Errorf("advertised thumbnail_url content type = %q, want %q", ct, thumbnail.ContentType)
	}
}

// A video whose platform published no poster is still an archived page, and
// that page was screenshotted before yt-dlp ever ran. The manifest must report
// that image rather than claim the archive has no preview.
func TestServeVideoManifestFallsBackToSiblingScreenshotThumbnail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm06", "https://www.youtube.com/watch?v=noposter", map[string]string{
		"yt-dlp":     "completed",
		"screenshot": "completed",
	})

	// The video item captured no poster; the screenshot did.
	shotBytes := []byte("\xff\xd8\xff\xe0 screenshot jpeg")
	storeTestObject(t, store, "thm06/screenshot-abcd-thumb.jpg", shotBytes)
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").
		Update("storage_key", "thm06/yt-dlp-abcd.mp4").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "screenshot").
		Updates(map[string]interface{}{
			"storage_key":      "thm06/screenshot-abcd.png",
			"thumbnail_key":    "thm06/screenshot-abcd-thumb.jpg",
			"thumbnail_status": models.ThumbnailStatusReady,
		}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/thumb/:shortid", func(c *gin.Context) { ServeThumbnail(c, store, db, nil) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm06/manifest", nil))
	manifest := decodeThumbnailManifest(t, rec.Body.Bytes())
	if manifest.ThumbnailURL == nil || *manifest.ThumbnailURL != "/thumb/thm06" {
		t.Fatalf("thumbnail_url = %v, want /thumb/thm06", manifest.ThumbnailURL)
	}

	thumbRec := httptest.NewRecorder()
	router.ServeHTTP(thumbRec, httptest.NewRequest(http.MethodGet, *manifest.ThumbnailURL, nil))
	if got := thumbRec.Body.Bytes(); string(got) != string(shotBytes) {
		t.Errorf("thumbnail_url served %q, want the sibling screenshot bytes", got)
	}
}

// The absent case the consumer must be able to tell apart from "you are on an
// old schema": the key is present and explicitly null.
func TestServeVideoManifestReportsNullThumbnailWhenNoneStored(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm02", "https://www.youtube.com/watch?v=nothumb", map[string]string{"yt-dlp": "completed"})
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").
		Update("storage_key", "thm02/yt-dlp-abcd.mp4").Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm02/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The raw body must carry the key even though it is false, so absence is
	// distinguishable from an older Arker that never had the field.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	value, present := raw["thumbnail_url"]
	if !present {
		t.Fatal("thumbnail_url is omitted; it must be present and null so absence is distinguishable from an older schema")
	}
	if string(value) != "null" {
		t.Errorf("thumbnail_url = %s, want null", value)
	}
}

// A capture still running has nothing stored to preview yet.
func TestServeVideoManifestReportsNoThumbnailForPendingCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm03", "https://www.youtube.com/watch?v=pending", map[string]string{"yt-dlp": "pending"})

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm03/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	manifest := decodeThumbnailManifest(t, rec.Body.Bytes())
	if manifest.ThumbnailURL != nil {
		t.Errorf("pending capture advertised a thumbnail: %+v", manifest)
	}
}

// TestServeVideoManifestIgnoresUnreadyThumbnailRow guards against advertising a
// URL that would serve the placeholder: a row mid-generation has a status but
// no stored object yet.
func TestServeVideoManifestIgnoresUnreadyThumbnailRow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm04", "https://www.youtube.com/watch?v=queued", map[string]string{"yt-dlp": "completed"})
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").
		Updates(map[string]interface{}{
			"storage_key":      "thm04/yt-dlp-abcd.mp4",
			"thumbnail_status": models.ThumbnailStatusPending,
		}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm04/manifest", nil))
	manifest := decodeThumbnailManifest(t, rec.Body.Bytes())
	if manifest.ThumbnailURL != nil {
		t.Errorf("a pending thumbnail row was advertised as available: %+v", manifest)
	}
}

func TestServeVideoManifestDoesNotSynthesizeLegacyMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "old01", "https://www.youtube.com/watch?v=legacy", map[string]string{"youtube": "completed"})
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "youtube").Update("storage_key", "old01/youtube.mp4").Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/old01/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var manifest struct {
		CaptureStatus             string          `json:"capture_status"`
		MediaURL                  *string         `json:"media_url"`
		MetadataAvailable         bool            `json:"metadata_available"`
		Metadata                  json.RawMessage `json:"metadata"`
		MetadataUnavailableReason string          `json:"metadata_unavailable_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CaptureStatus != "completed" || manifest.MetadataAvailable || string(manifest.Metadata) != "null" {
		t.Errorf("legacy manifest synthesized metadata: %+v", manifest)
	}
	if manifest.MetadataUnavailableReason != "legacy_archive_without_structured_metadata" {
		t.Errorf("legacy reason = %q", manifest.MetadataUnavailableReason)
	}
	if manifest.MediaURL == nil || *manifest.MediaURL != "/archive/old01/yt-dlp" {
		t.Errorf("legacy media URL = %v", manifest.MediaURL)
	}
}

// A legacy row still typed "youtube" is served under the canonical yt-dlp
// segment everywhere else in the manifest, and the thumbnail URL must
// normalize the same way or it would advertise a path that renders a
// placeholder.
func TestServeVideoManifestThumbnailUsesCanonicalTypeForLegacyRows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "thm05", "https://www.youtube.com/watch?v=legacy", map[string]string{"youtube": "completed"})

	thumbKey := "thm05/youtube-abcd-thumb.jpg"
	thumbBytes := []byte("\xff\xd8\xff\xe0 legacy jpeg")
	storeTestObject(t, store, thumbKey, thumbBytes)
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "youtube").
		Updates(map[string]interface{}{
			"storage_key":      "thm05/youtube.mp4",
			"thumbnail_key":    thumbKey,
			"thumbnail_status": models.ThumbnailStatusReady,
		}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/thumb/:shortid", func(c *gin.Context) { ServeThumbnail(c, store, db, nil) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/thm05/manifest", nil))
	manifest := decodeThumbnailManifest(t, rec.Body.Bytes())
	if manifest.ThumbnailURL == nil || *manifest.ThumbnailURL != "/thumb/thm05" {
		t.Fatalf("thumbnail_url = %v, want /thumb/thm05", manifest.ThumbnailURL)
	}

	thumbRec := httptest.NewRecorder()
	router.ServeHTTP(thumbRec, httptest.NewRequest(http.MethodGet, *manifest.ThumbnailURL, nil))
	if got := thumbRec.Body.Bytes(); string(got) != string(thumbBytes) {
		t.Errorf("canonical thumbnail URL served %q, want the legacy row's stored bytes", got)
	}
}
