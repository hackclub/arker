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
	for _, key := range []string{"duration_seconds", "title", "channel", "publication_timestamp"} {
		if _, ok := normalized[key]; !ok {
			t.Errorf("normalized metadata missing required key %q: %v", key, normalized)
		}
	}
	media, _ := normalized["media"].(map[string]interface{})
	for _, key := range []string{"width", "height"} {
		if _, ok := media[key]; !ok {
			t.Errorf("normalized media missing required key %q: %v", key, media)
		}
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

func TestNormalizeVideoManifestMetadataRecoversProviderAuthoredChannelIDs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "yt-dlp author id", raw: `{"author_id":"@l.a.c.l.u.s.t.r","media":{}}`, want: "l.a.c.l.u.s.t.r"},
		{name: "TikTok profile URL", raw: `{"source_url":"https://www.tiktok.com/@worldguessr_/","media":{}}`, want: "worldguessr_"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var metadata map[string]interface{}
			if err := json.Unmarshal(normalizeVideoManifestMetadata(json.RawMessage(tc.raw)), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["channel"] != tc.want {
				t.Fatalf("channel = %#v, want %q", metadata["channel"], tc.want)
			}
		})
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

// A gallery capture remains a gallery even when its only card is a video. The
// consumer contract learns the kind by requiring exactly one of the video
// manifest and gallery raw endpoints to answer 200.
func TestVideoContractDoesNotProjectSingleVideoGalleryCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "igr01", "completed")
	writeGalleryZipFixture(t, db, store, "igr01", []struct{ name, body string }{
		{"metadata.json", `{"file_count":1,"files":[{"name":"001.mp4","content_type":"video/mp4","is_video":true}],"completeness":{"state":"complete","expected":1,"stored":1}}`},
		{"001.mp4", "video"},
		{"brightdata.json", `{"content_type":"Reel"}`},
	})
	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/gallery/:shortid/raw", func(c *gin.Context) { ServeGalleryRawMetadata(c, store, db) })
	video := httptest.NewRecorder()
	router.ServeHTTP(video, httptest.NewRequest(http.MethodGet, "/video/igr01/manifest", nil))
	if video.Code != http.StatusNotFound {
		t.Fatalf("video manifest status = %d, want 404: %s", video.Code, video.Body.String())
	}
	raw := httptest.NewRecorder()
	router.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "/gallery/igr01/raw", nil))
	if raw.Code != http.StatusOK {
		t.Fatalf("gallery raw status = %d, want 200: %s", raw.Code, raw.Body.String())
	}
}

func TestVideoContractDoesNotProjectPhotoOrCarouselGalleryCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		entries []struct{ name, body string }
	}{
		{name: "photo", entries: []struct{ name, body string }{
			{"metadata.json", `{"file_count":1,"files":[{"name":"001.jpg","content_type":"image/jpeg","is_video":false}],"completeness":{"state":"complete","expected":1,"stored":1}}`},
			{"001.jpg", "image-bytes"},
		}},
		{name: "carousel", entries: []struct{ name, body string }{
			{"metadata.json", `{"file_count":2,"files":[{"name":"001.mp4","content_type":"video/mp4","is_video":true},{"name":"002.jpg","content_type":"image/jpeg","is_video":false}],"completeness":{"state":"complete","expected":2,"stored":2}}`},
			{"001.mp4", "video-bytes"}, {"002.jpg", "image-bytes"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newHandlerLogTestDB(t)
			store := storage.NewMemoryStorage()
			seedGalleryCapture(t, db, store, "nonv1", "completed")
			writeGalleryZipFixture(t, db, store, "nonv1", tc.entries)
			router := gin.New()
			router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/nonv1/manifest", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s; a photo/carousel must remain a gallery", rec.Code, rec.Body.String())
			}
		})
	}
}
