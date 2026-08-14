package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	entries := []struct{ name, body string }{
		{"metadata.json", `{"source_url":"https://www.instagram.com/p/ABC123/","extractor":"instagram","author":"someone","author_name":"Some One","description":"a caption","file_count":2}`},
		{"001.jpg", "jpeg-bytes"},
		{"001.jpg.json", `{"width":1080,"height":1350}`},
		{"002.mp4", "mp4-bytes"},
		{"002.mp4.json", `{"width":720,"height":1280}`},
	}
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
	return buf.Bytes()
}

func seedGalleryCapture(t *testing.T, db *gorm.DB, storageInstance storage.Storage, shortID, status string) {
	t.Helper()

	key := "archive/" + shortID + "/gallery-dl.zip"
	if status == "completed" {
		writer, err := storageInstance.Writer(key)
		if err != nil {
			t.Fatalf("storage writer: %v", err)
		}
		if _, err := writer.Write(buildGalleryArchive(t)); err != nil {
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
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
}

func newGalleryRouter(db *gorm.DB, storageInstance storage.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/gallery/:shortid/list", func(c *gin.Context) { ServeGalleryManifest(c, storageInstance, db) })
	r.GET("/gallery/:shortid/file/*filepath", func(c *gin.Context) { ServeGalleryFile(c, storageInstance, db) })
	r.GET("/gallery/:shortid/raw", func(c *gin.Context) { ServeGalleryRawMetadata(c, storageInstance, db) })
	return r
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

func TestServeGalleryManifestReturnsMetadataAndMediaOnly(t *testing.T) {
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
