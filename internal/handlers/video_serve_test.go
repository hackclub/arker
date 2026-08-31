package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
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

// Instagram serves a Reel under both /p/ and /reel/. URL-shape routing sends
// the /p/ spelling through gallery-dl, so the response layer must project a
// complete, single-video gallery bundle onto the normal video contract.
func TestVideoContractProjectsSingleVideoGalleryCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "igr01", "completed")
	video := append([]byte{0, 0, 0, 20}, []byte("ftypisomarchived-reel-bytes")...)
	writeGalleryZipFixture(t, db, store, "igr01", []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","extractor":"instagram","post_id":"ABC123","post_url":"https://www.instagram.com/p/ABC123/","author":"someone","title":"A reel","description":"caption","date":"2026-08-09T22:16:38Z","views":123,"likes":42,"comments":5,"file_count":1,"files":[{"name":"001.mp4","size":32,"content_type":"video/mp4","is_video":true,"width":576,"height":1024,"duration_seconds":48.087074}],"completeness":{"state":"complete","expected":1,"stored":1},"archived_at":"2026-08-16T18:31:22Z"}`},
		{"001.mp4", string(video)},
		{"brightdata.json", `{"content_type":"Reel","product_type":"clips","shortcode":"ABC123"}`},
	})
	if err := db.Model(&models.ArchiveItem{}).
		Where("type = ?", utils.ArchiveTypeGalleryDl).
		Update("source", models.ArchiveSourceBrightData).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/video/:shortid/raw", func(c *gin.Context) { ServeVideoRawMetadata(c, store, db) })
	router.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, store, db) })
	router.HEAD("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, store, db) })

	manifestRec := httptest.NewRecorder()
	router.ServeHTTP(manifestRec, httptest.NewRequest(http.MethodGet, "/video/igr01/manifest", nil))
	if manifestRec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", manifestRec.Code, manifestRec.Body.String())
	}
	var manifest struct {
		CaptureStatus     string          `json:"capture_status"`
		MediaURL          *string         `json:"media_url"`
		MetadataAvailable bool            `json:"metadata_available"`
		Metadata          json.RawMessage `json:"metadata"`
		RawMetadataURL    *string         `json:"raw_metadata_url"`
		Provenance        string          `json:"provenance"`
	}
	if err := json.Unmarshal(manifestRec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CaptureStatus != "completed" || !manifest.MetadataAvailable {
		t.Fatalf("manifest = %+v, want completed video metadata", manifest)
	}
	if manifest.MediaURL == nil || *manifest.MediaURL != "/archive/igr01/yt-dlp" {
		t.Errorf("media_url = %v, want the stable video URL", manifest.MediaURL)
	}
	if manifest.RawMetadataURL == nil || *manifest.RawMetadataURL != "/video/igr01/raw" {
		t.Errorf("raw_metadata_url = %v, want the stored provider record", manifest.RawMetadataURL)
	}
	if manifest.Provenance != models.ArchiveSourceBrightData {
		t.Errorf("provenance = %q, want brightdata", manifest.Provenance)
	}
	var metadata struct {
		Platform        string   `json:"platform"`
		PostID          string   `json:"post_id"`
		Title           string   `json:"title"`
		DurationSeconds *float64 `json:"duration_seconds"`
		Engagement      struct {
			Views    *int64 `json:"views"`
			Likes    *int64 `json:"likes"`
			Comments *int64 `json:"comments"`
		} `json:"engagement"`
		Media struct {
			ContentType string `json:"content_type"`
			Width       *int64 `json:"width"`
			Height      *int64 `json:"height"`
		} `json:"media"`
	}
	if err := json.Unmarshal(manifest.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Platform != "instagram" || metadata.PostID != "ABC123" || metadata.Title != "A reel" || metadata.Media.ContentType != "video/mp4" {
		t.Errorf("normalized video metadata = %+v", metadata)
	}
	if metadata.Media.Width == nil || *metadata.Media.Width != 576 || metadata.Media.Height == nil || *metadata.Media.Height != 1024 {
		t.Errorf("video dimensions = %v x %v, want 576x1024", metadata.Media.Width, metadata.Media.Height)
	}
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 48.087074 {
		t.Errorf("duration_seconds = %v, want 48.087074", metadata.DurationSeconds)
	}
	if metadata.Engagement.Views == nil || *metadata.Engagement.Views != 123 ||
		metadata.Engagement.Likes == nil || *metadata.Engagement.Likes != 42 ||
		metadata.Engagement.Comments == nil || *metadata.Engagement.Comments != 5 {
		t.Errorf("engagement = %+v, want views/likes/comments 123/42/5", metadata.Engagement)
	}

	videoRec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/archive/igr01/youtube", nil)
	request.Header.Set("Range", "bytes=4-11")
	router.ServeHTTP(videoRec, request)
	if videoRec.Code != http.StatusPartialContent {
		t.Fatalf("video status = %d, body = %s", videoRec.Code, videoRec.Body.String())
	}
	if got := videoRec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	if !bytes.Equal(videoRec.Body.Bytes(), video[4:12]) {
		t.Errorf("range body = %q, want %q", videoRec.Body.Bytes(), video[4:12])
	}
	headRec := httptest.NewRecorder()
	router.ServeHTTP(headRec, httptest.NewRequest(http.MethodHead, "/archive/igr01/yt-dlp", nil))
	if headRec.Code != http.StatusOK || headRec.Header().Get("Content-Type") != "video/mp4" || headRec.Header().Get("Content-Length") != "31" {
		t.Errorf("HEAD status=%d type=%q length=%q, want the projected MP4 metadata", headRec.Code, headRec.Header().Get("Content-Type"), headRec.Header().Get("Content-Length"))
	}
	if headRec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", headRec.Body.Len())
	}

	rawRec := httptest.NewRecorder()
	router.ServeHTTP(rawRec, httptest.NewRequest(http.MethodGet, "/video/igr01/raw", nil))
	if rawRec.Code != http.StatusOK {
		t.Fatalf("raw status = %d, body = %s", rawRec.Code, rawRec.Body.String())
	}
	var raw struct {
		Provider string `json:"provider"`
		Records  []struct {
			Metadata map[string]interface{} `json:"metadata"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rawRec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Provider != "brightdata" || len(raw.Records) != 1 || raw.Records[0].Metadata["content_type"] != "Reel" {
		t.Errorf("raw provider record = %+v", raw)
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
