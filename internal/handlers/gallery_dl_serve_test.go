package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/testfixtures"
	"arker/internal/utils"
)

// buildGalleryArchive produces a ZIP shaped exactly like the one
// GalleryDLArchiver stores: Arker's metadata.json, the media files, and
// gallery-dl's raw per-file sidecars.
func buildGalleryArchive(t *testing.T) []byte {
	t.Helper()
	return buildGalleryBundle(t, [][2]string{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","extractor":"instagram","author":"someone","author_name":"Some One","description":"a caption","file_count":2}`},
		{"001.jpg", "jpeg-bytes"},
		{"001.jpg.json", `{"width":1080,"height":1350}`},
		{"002.mp4", "mp4-bytes"},
		{"002.mp4.json", `{"width":720,"height":1280}`},
	})
}

// buildGalleryBundle zips the given name/body pairs in order.
func buildGalleryBundle(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		w, err := zw.Create(entry[0])
		if err != nil {
			t.Fatalf("create %s: %v", entry[0], err)
		}
		if _, err := w.Write([]byte(entry[1])); err != nil {
			t.Fatalf("write %s: %v", entry[0], err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func seedGalleryCapture(t *testing.T, db *gorm.DB, storageInstance storage.Storage, shortID, status string) {
	t.Helper()
	seedGalleryBundle(t, db, storageInstance, shortID, status, "", buildGalleryArchive(t))
}

// seedGalleryBundle stores an arbitrary bundle for a capture, so a test can
// describe the exact provider record whose shape it cares about.
func seedGalleryBundle(t *testing.T, db *gorm.DB, storageInstance storage.Storage, shortID, status, source string, bundle []byte) {
	t.Helper()

	key := "archive/" + shortID + "/gallery-dl.zip"
	if status == "completed" {
		writer, err := storageInstance.Writer(key)
		if err != nil {
			t.Fatalf("storage writer: %v", err)
		}
		if _, err := writer.Write(bundle); err != nil {
			t.Fatalf("storage write: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("storage close: %v", err)
		}
	}

	createVideoCapture(t, db, shortID, "https://www.instagram.com/p/"+shortID+"/", nil)

	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		t.Fatalf("find capture: %v", err)
	}
	item := models.ArchiveItem{
		CaptureID:  capture.ID,
		Type:       utils.ArchiveTypeGalleryDl,
		Status:     status,
		StorageKey: key,
		Extension:  ".zip",
		Source:     source,
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
}

func newGalleryRouter(db *gorm.DB, storageInstance storage.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/gallery/:shortid/list", func(c *gin.Context) { ServeGalleryList(c, storageInstance, db) })
	r.GET("/gallery/:shortid/manifest", func(c *gin.Context) { ServeGalleryManifest(c, storageInstance, db) })
	r.GET("/gallery/:shortid/file/*filepath", func(c *gin.Context) { ServeGalleryFile(c, storageInstance, db) })
	r.GET("/gallery/:shortid/raw", func(c *gin.Context) { ServeGalleryRawMetadata(c, storageInstance, db) })
	return r
}

func TestGalleryRawIsStableKindContractAndNormalizesRecords(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, store, "raw01", "completed")
	router := newGalleryRouter(db, store)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gallery/raw01/raw", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ShortID       string `json:"short_id"`
		CaptureStatus string `json:"capture_status"`
		Records       []struct {
			MediaURL  string                 `json:"media_url"`
			MediaURLs []string               `json:"media_urls"`
			Metadata  map[string]interface{} `json:"metadata"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ShortID != "raw01" || body.CaptureStatus != "completed" || len(body.Records) != 2 {
		t.Fatalf("raw envelope = %+v", body)
	}
	for i, record := range body.Records {
		for _, key := range []string{"caption", "user_posted", "date_posted", "photos_number", "likes", "num_comments", "media_url", "media_urls"} {
			if _, ok := record.Metadata[key]; !ok {
				t.Errorf("records[%d].metadata missing %q: %v", i, key, record.Metadata)
			}
		}
		if record.MediaURL == "" || len(record.MediaURLs) != 1 || !strings.HasPrefix(record.MediaURL, "http://example.com/gallery/raw01/file/") {
			t.Errorf("records[%d] media URLs = %q / %v", i, record.MediaURL, record.MediaURLs)
		}
	}
	if body.Records[0].Metadata["caption"] != "a caption" || body.Records[0].Metadata["user_posted"] != "someone" {
		t.Errorf("normalized metadata = %v", body.Records[0].Metadata)
	}

	seedGalleryCapture(t, db, store, "pend1", "pending")
	pending := httptest.NewRecorder()
	router.ServeHTTP(pending, httptest.NewRequest(http.MethodGet, "/gallery/pend1/raw", nil))
	if pending.Code != http.StatusOK {
		t.Fatalf("pending gallery status = %d, want 200", pending.Code)
	}
	var pendingBody map[string]interface{}
	_ = json.Unmarshal(pending.Body.Bytes(), &pendingBody)
	if pendingBody["capture_status"] != "pending" {
		t.Errorf("pending body = %v", pendingBody)
	}

	unknown := httptest.NewRecorder()
	router.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/gallery/nope1/raw", nil))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown gallery status = %d, want 404", unknown.Code)
	}
}

func TestGalleryCanonicalizesMisleadingJPEGExtension(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedRealGalleryCapture(t, db, storageInstance, "mime1", "instagram_image", testfixtures.GalleryDlFake{
		ImageExtension: ".heic",
	})
	router := newGalleryRouter(db, storageInstance)

	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/gallery/mime1/list", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var listBody struct {
		Metadata struct {
			Files []struct {
				Name         string `json:"name"`
				ContentType  string `json:"content_type"`
				MetadataFile string `json:"metadata_file"`
			} `json:"files"`
		} `json:"metadata"`
		Files []struct {
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
			URL         string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listBody.Metadata.Files) != 1 {
		t.Fatalf("metadata files = %+v, want one file", listBody.Metadata.Files)
	}
	metadataFile := listBody.Metadata.Files[0]
	if metadataFile.Name != "001.jpg" || metadataFile.ContentType != "image/jpeg" || metadataFile.MetadataFile != "001.jpg.json" {
		t.Errorf("metadata file = %+v, want canonical 001.jpg, image/jpeg, and 001.jpg.json", metadataFile)
	}
	if len(listBody.Files) != 1 {
		t.Fatalf("listed files = %+v, want one file", listBody.Files)
	}
	listedFile := listBody.Files[0]
	if listedFile.Name != "001.jpg" || listedFile.ContentType != "image/jpeg" || listedFile.URL != "/gallery/mime1/file/001.jpg" {
		t.Errorf("listed file = %+v, want canonical JPEG name, type, and URL", listedFile)
	}

	fileRec := httptest.NewRecorder()
	router.ServeHTTP(fileRec, httptest.NewRequest(http.MethodGet, listedFile.URL, nil))
	if fileRec.Code != http.StatusOK {
		t.Fatalf("file status = %d, body = %s", fileRec.Code, fileRec.Body.String())
	}
	if got := fileRec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("file Content-Type = %q, want image/jpeg", got)
	}
	if !bytes.HasPrefix(fileRec.Body.Bytes(), []byte{0xff, 0xd8, 0xff}) {
		t.Errorf("file body does not begin with JPEG magic: %x", fileRec.Body.Bytes()[:min(8, fileRec.Body.Len())])
	}

	rawRec := httptest.NewRecorder()
	router.ServeHTTP(rawRec, httptest.NewRequest(http.MethodGet, "/gallery/mime1/raw", nil))
	if rawRec.Code != http.StatusOK {
		t.Fatalf("raw status = %d, body = %s", rawRec.Code, rawRec.Body.String())
	}
	var rawBody struct {
		Records []struct {
			Filename string `json:"filename"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rawRec.Body.Bytes(), &rawBody); err != nil {
		t.Fatalf("decode raw metadata: %v", err)
	}
	if len(rawBody.Records) != 1 || rawBody.Records[0].Filename != "001.jpg.json" {
		t.Errorf("raw metadata records = %+v, want canonical 001.jpg.json sidecar", rawBody.Records)
	}

	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", "mime1", utils.ArchiveTypeGalleryDl).
		First(&item).Error; err != nil {
		t.Fatalf("load gallery item: %v", err)
	}
	archiveReader, err := storageInstance.Reader(item.StorageKey)
	if err != nil {
		t.Fatalf("open stored ZIP: %v", err)
	}
	archiveBytes, err := io.ReadAll(archiveReader)
	archiveReader.Close()
	if err != nil {
		t.Fatalf("read stored ZIP: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		t.Fatalf("open stored ZIP: %v", err)
	}
	entries := make(map[string]bool, len(zr.File))
	for _, entry := range zr.File {
		entries[entry.Name] = true
	}
	for _, name := range []string{"metadata.json", "001.jpg", "001.jpg.json"} {
		if !entries[name] {
			t.Errorf("stored ZIP entries = %v, missing %s", entries, name)
		}
	}
	for name := range entries {
		if name == "001.heic" || name == "001.heic.json" {
			t.Errorf("stored ZIP retains misleading entry %s", name)
		}
	}
}

func TestServeGalleryFileSniffsLegacyMisleadingExtension(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "old01", "completed")

	var item models.ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", "old01", utils.ArchiveTypeGalleryDl).
		First(&item).Error; err != nil {
		t.Fatalf("load gallery item: %v", err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{"metadata.json", []byte(`{"files":[{"name":"001.heic","content_type":"application/octet-stream"}]}`)},
		{"001.heic", testfixtures.PlaceholderJPEG(t, 16, 16)},
	} {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatalf("create %s: %v", entry.name, err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatalf("write %s: %v", entry.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}
	w, err := storageInstance.Writer(item.StorageKey)
	if err != nil {
		t.Fatalf("replace stored ZIP: %v", err)
	}
	if _, err := w.Write(archive.Bytes()); err != nil {
		t.Fatalf("write stored ZIP: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close stored ZIP: %v", err)
	}

	router := newGalleryRouter(db, storageInstance)
	for _, requestPath := range []string{"/gallery/old01/list", "/gallery/old01/file/001.heic"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", requestPath, rec.Code, rec.Body.String())
		}
		if requestPath == "/gallery/old01/list" {
			var body struct {
				Files []struct {
					ContentType string `json:"content_type"`
				} `json:"files"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			if len(body.Files) != 1 || body.Files[0].ContentType != "image/jpeg" {
				t.Errorf("listed legacy file = %+v, want image/jpeg sniffed from bytes", body.Files)
			}
		} else if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
			t.Errorf("served legacy file Content-Type = %q, want image/jpeg sniffed from bytes", got)
		}
	}
}

func TestServeGalleryListReturnsMetadataAndMediaOnly(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "7fbf9", "completed")

	req := httptest.NewRequest(http.MethodGet, "/gallery/7fbf9/list", nil)
	rec := httptest.NewRecorder()
	newGalleryRouter(db, storageInstance).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Metadata map[string]interface{} `json:"metadata"`
		Files    []struct {
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
			URL         string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Metadata["author"] != "someone" {
		t.Errorf("metadata.author = %v, want someone", body.Metadata["author"])
	}
	if body.Metadata["description"] != "a caption" {
		t.Errorf("metadata.description = %v, want the caption", body.Metadata["description"])
	}

	// JSON sidecars and metadata.json must not be offered as gallery media.
	if len(body.Files) != 2 {
		t.Fatalf("files = %+v, want exactly the 2 media files", body.Files)
	}
	if body.Files[0].Name != "001.jpg" || body.Files[0].ContentType != "image/jpeg" {
		t.Errorf("files[0] = %+v, want 001.jpg as image/jpeg", body.Files[0])
	}
	if body.Files[1].ContentType != "video/mp4" {
		t.Errorf("files[1] content type = %q, want video/mp4", body.Files[1].ContentType)
	}
	if body.Files[0].URL != "/gallery/7fbf9/file/001.jpg" {
		t.Errorf("files[0].URL = %q, want /gallery/7fbf9/file/001.jpg", body.Files[0].URL)
	}
	if body.Files[0].Size != int64(len("jpeg-bytes")) {
		t.Errorf("files[0].Size = %d, want %d", body.Files[0].Size, len("jpeg-bytes"))
	}
}

func TestServeGalleryFileServesMediaWithCorrectType(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "7fbf9", "completed")
	router := newGalleryRouter(db, storageInstance)

	req := httptest.NewRequest(http.MethodGet, "/gallery/7fbf9/file/001.jpg", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "jpeg-bytes" {
		t.Errorf("body = %q, want jpeg-bytes", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	// Archived media is attacker-controlled, so the browser must not be
	// allowed to sniff a different (possibly executable) type.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestServeGalleryFileRejectsTraversalAndMissingFiles(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "7fbf9", "completed")
	router := newGalleryRouter(db, storageInstance)

	for _, path := range []string{
		"/gallery/7fbf9/file/../../etc/passwd",
		"/gallery/7fbf9/file/nested/001.jpg",
		"/gallery/7fbf9/file/999.jpg",
		"/gallery/7fbf9/file/",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// An archive that has not finished (or does not exist) must not be browsable.
func TestServeGalleryRejectsIncompleteAndUnknownCaptures(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "pend1", "pending")
	router := newGalleryRouter(db, storageInstance)

	for _, path := range []string{
		"/gallery/pend1/list",
		"/gallery/pend1/file/001.jpg",
		"/gallery/nope1/list",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// Safari and iOS open a video with "Range: bytes=0-1" and refuse to play
// against a 200, and seeking is broken everywhere without range support.
func TestServeGalleryFileSupportsRangeRequests(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "7fbf9", "completed")
	router := newGalleryRouter(db, storageInstance)

	req := httptest.NewRequest(http.MethodGet, "/gallery/7fbf9/file/002.mp4", nil)
	req.Header.Set("Range", "bytes=0-2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 Partial Content", rec.Code)
	}
	if got := rec.Body.String(); got != "mp4" {
		t.Errorf("body = %q, want the first 3 bytes (%q)", got, "mp4")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-2/9" {
		t.Errorf("Content-Range = %q, want bytes 0-2/9", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
}

func TestServeGalleryFileAdvertisesRangeSupport(t *testing.T) {
	db := newHandlerLogTestDB(t)
	storageInstance := storage.NewMemoryStorage()
	seedGalleryCapture(t, db, storageInstance, "7fbf9", "completed")
	router := newGalleryRouter(db, storageInstance)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gallery/7fbf9/file/001.jpg", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Body.String(); got != "jpeg-bytes" {
		t.Errorf("body = %q, want the full file when no Range is sent", got)
	}
}

// TestGalleryRawCoercesProviderShapesToContractTypes pins the promise the raw
// endpoint's normalized fields make: caption and user_posted are strings,
// date_posted is a timestamp string, counts are numbers — whatever shape the
// provider that produced the bundle happened to use. Ten Instagram galleries
// re-captured through the Apify fallback on 2026-09-03 served the provider's
// caption object and an epoch integer instead, leaving consumers that map
// caption to a title and date_posted to a published-at with neither.
func TestGalleryRawCoercesProviderShapesToContractTypes(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	router := newGalleryRouter(db, store)

	const normalizedMetadata = `{"source_url":"https://www.instagram.com/p/ABC123/","extractor":"instagram","subcategory":"apify",` +
		`"author":"starthackclub","author_name":"Hack Club","description":"normalized caption","date":"2026-08-28T01:00:42Z","likes":794,"comments":23,"file_count":1}`

	cases := []struct {
		name    string
		shortID string
		record  string
		want    map[string]interface{}
	}{
		{
			// Instagram through Apify: caption is Instagram's own caption
			// object and taken_at is epoch seconds.
			name:    "instagram fallback record",
			shortID: "rawig",
			record: `{"code":"ABC123","caption":{"pk":"18361650220301169","text":"Next Station 🚇","hashtags":["#pinpaint"],"mentions":["@apple.cheeks"]},` +
				`"taken_at":1776344250,"like_count":794,"comment_count":23}`,
			want: map[string]interface{}{
				"caption":       "Next Station 🚇",
				"user_posted":   "starthackclub",
				"date_posted":   "2026-04-16T12:57:30Z",
				"likes":         float64(794),
				"num_comments":  float64(23),
				"photos_number": float64(1),
			},
		},
		{
			// X through Apify: the poster is a user object and the date is
			// Ruby's layout.
			name:    "x fallback record",
			shortID: "rawxx",
			record: `{"id":"1","text":"a tweet","author":{"type":"user","userName":"icyelectronics","name":"Cyao"},` +
				`"createdAt":"Sat Jun 06 07:21:57 +0000 2026","date":"Sat Jun 06 07:21:57 +0000 2026","likes":"1,024"}`,
			want: map[string]interface{}{
				"caption":     "a tweet",
				"user_posted": "icyelectronics",
				"date_posted": "2026-06-06T07:21:57Z",
				"likes":       float64(1024),
			},
		},
		{
			// Facebook through Apify: the owner object names nobody, so the
			// normalized author answers rather than an opaque numeric id.
			name:    "facebook fallback record",
			shortID: "rawfb",
			record:  `{"post_id":"9","text":"a post","owner":{"__typename":"User","id":"100064643981468"},"timestamp":1787878842,"likes":25,"comments":0}`,
			want: map[string]interface{}{
				"caption":      "a post",
				"user_posted":  "starthackclub",
				"date_posted":  "2026-08-28T01:00:42Z",
				"likes":        float64(25),
				"num_comments": float64(0),
			},
		},
		{
			// A native gallery-dl sidecar keeps reporting exactly what it
			// always did, in the one timestamp format the field promises.
			name:    "native gallery-dl sidecar",
			shortID: "rawgd",
			record:  `{"username":"someone","description":"a caption","date":"2026-08-28 01:00:42","likes":12,"comments":3}`,
			want: map[string]interface{}{
				"caption":      "a caption",
				"user_posted":  "someone",
				"date_posted":  "2026-08-28T01:00:42Z",
				"likes":        float64(12),
				"num_comments": float64(3),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedGalleryBundle(t, db, store, tc.shortID, "completed", models.ArchiveSourceApify, buildGalleryBundle(t, [][2]string{
				{"metadata.json", normalizedMetadata},
				{"001.jpg", "jpeg-bytes"},
				{"apify.json", tc.record},
			}))

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gallery/"+tc.shortID+"/raw", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Records []struct {
					Metadata map[string]interface{} `json:"metadata"`
				} `json:"records"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Records) != 1 {
				t.Fatalf("records = %d, want 1", len(body.Records))
			}
			metadata := body.Records[0].Metadata
			for field, want := range tc.want {
				if got := metadata[field]; got != want {
					t.Errorf("metadata[%q] = %#v (%T), want %#v", field, got, got, want)
				}
			}
		})
	}
}

// TestGalleryRawKeepsFlattenedProviderValues checks that reducing a structured
// provider field to the contract's scalar does not delete it: /raw serves
// provider sidecars, and Instagram's mentions list lives only inside the
// caption object it replaces.
func TestGalleryRawKeepsFlattenedProviderValues(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	router := newGalleryRouter(db, store)

	seedGalleryBundle(t, db, store, "rawkp", "completed", models.ArchiveSourceApify, buildGalleryBundle(t, [][2]string{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","author":"starthackclub","file_count":1}`},
		{"001.jpg", "jpeg-bytes"},
		{"apify.json", `{"caption":{"pk":"1","text":"hello","mentions":["@somebody"]},"taken_at":1776344250}`},
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gallery/rawkp/raw", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Records []struct {
			Metadata map[string]interface{} `json:"metadata"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(body.Records))
	}
	details, ok := body.Records[0].Metadata["caption_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("caption_details = %#v, want the provider's caption object", body.Records[0].Metadata["caption_details"])
	}
	if details["text"] != "hello" {
		t.Errorf("caption_details.text = %#v", details["text"])
	}
	mentions, ok := details["mentions"].([]interface{})
	if !ok || len(mentions) != 1 || mentions[0] != "@somebody" {
		t.Errorf("caption_details.mentions = %#v", details["mentions"])
	}
}
