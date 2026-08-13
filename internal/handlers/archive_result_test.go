package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/storage"
)

func resultRouter(db *gorm.DB, store storage.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/archive/:shortid", func(c *gin.Context) { ApiArchiveResult(c, store, db) })
	r.GET("/gallery/:shortid/raw", func(c *gin.Context) { ServeGalleryRawMetadata(c, store, db) })
	return r
}

func getResult(t *testing.T, r http.Handler, id string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/"+id, nil)
	req.Host = "archive.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	r.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestApiArchiveResultBasicStates(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	r := resultRouter(db, store)
	createVideoCapture(t, db, "plain", "https://example.com/article", map[string]string{"mhtml": "completed", "screenshot": "processing"})
	code, body := getResult(t, r, "plain")
	if code != 200 || body["social_post"] != nil || body["capture_done"] != false {
		t.Fatalf("ordinary result = %#v (status %d)", body, code)
	}
	if len(body["items"].([]any)) != 2 {
		t.Fatalf("items = %#v", body["items"])
	}
	createVideoCapture(t, db, "fail1", "https://youtu.be/abc", map[string]string{"yt-dlp": "failed"})
	_, failed := getResult(t, r, "fail1")
	social := failed["social_post"].(map[string]any)
	if social["status"] != "failed" || social["terminal"] != true || failed["capture_done"] != true {
		t.Fatalf("failed = %#v", failed)
	}
	if code, _ := getResult(t, r, "nope1"); code != 404 {
		t.Fatalf("unknown status = %d", code)
	}
}

func TestArchiveQueuedResponseIsAdditive(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/archive", nil)
	c.Request.Host = "archive.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	got := archiveQueuedResponse(c, "abc12")
	if got["url"] != "https://archive.test/abc12" || got["short_id"] != "abc12" || got["result_url"] != "https://archive.test/api/v1/archive/abc12" {
		t.Fatalf("response = %#v", got)
	}
}

func TestApiArchiveResultVideoProvidersAndLegacy(t *testing.T) {
	for _, tc := range []struct{ name, source, mode string }{{"native", "native", "primary"}, {"bright", "brightdata", "fallback"}} {
		t.Run(tc.name, func(t *testing.T) {
			db := newHandlerLogTestDB(t)
			store := storage.NewMemoryStorage()
			createVideoCapture(t, db, "vid01", "https://www.youtube.com/watch?v=x", map[string]string{"yt-dlp": "completed"})
			metaKey, rawKey := "vid/meta.json", "vid/raw.json"
			meta := `{"schema_version":"1","source_url":"https://www.youtube.com/watch?v=x","platform":"youtube","post_id":"x","canonical_url":"https://youtube.com/watch?v=x","title":"Title","description":"Text","author":"Display","uploader":"user","duration_seconds":12,"engagement":{"likes":2},"media":{"extension":".mp4","content_type":"video/mp4","size_bytes":8,"width":100,"height":200,"quality_label":"720p"},"archived_at":"2026-01-02T03:04:05Z","provenance":"` + tc.source + `","provider":"yt-dlp"}`
			storeTestObject(t, store, metaKey, []byte(meta))
			storeTestObject(t, store, rawKey, []byte(`{"cookie":"[REDACTED]"}`))
			db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").Updates(map[string]any{"storage_key": "vid/media.mp4", "file_size": 8, "metadata_key": metaKey, "raw_metadata_key": rawKey, "source": tc.source})
			if tc.source == models.ArchiveSourceBrightData {
				var item models.ArchiveItem
				db.Where("type = ?", "yt-dlp").First(&item)
				db.Create(&models.BrightDataUsage{ArchiveItemID: item.ID, ShortID: "vid01", Product: "browser_api", BytesTransferred: 1_000_000, CostUSD: 0.0084, Success: true})
				// Failed attempts can still be billable and must be included.
				db.Create(&models.BrightDataUsage{ArchiveItemID: item.ID, ShortID: "vid01", Product: "browser_api", BytesTransferred: 500_000, CostUSD: 0.0042, Success: false})
			}
			_, body := getResult(t, resultRouter(db, store), "vid01")
			social := body["social_post"].(map[string]any)
			if social["status"] != "fulfilled" || social["fulfilled"] != true {
				t.Fatalf("social = %#v", social)
			}
			media := social["media"].([]any)[0].(map[string]any)
			if !strings.HasPrefix(media["url"].(string), "https://archive.test/archive/vid01/") {
				t.Fatalf("media = %#v", media)
			}
			prov := social["provenance"].(map[string]any)
			if prov["source"] != tc.source || prov["mode"] != tc.mode {
				t.Fatalf("provenance = %#v", prov)
			}
			cost := body["cost"].(map[string]any)
			if tc.source == models.ArchiveSourceNative {
				if cost["total_usd"] != float64(0) || cost["estimated"] != false {
					t.Fatalf("native cost = %#v", cost)
				}
			} else if cost["total_usd"] != 0.0126 || cost["estimated"] != true || len(cost["breakdown"].([]any)) != 2 {
				t.Fatalf("Bright Data cost = %#v", cost)
			}
		})
	}
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "old01", "https://youtu.be/legacy", map[string]string{"youtube": "completed"})
	db.Model(&models.ArchiveItem{}).Where("type = ?", "youtube").Update("storage_key", "old/media.mp4")
	_, old := getResult(t, resultRouter(db, store), "old01")
	failure := old["social_post"].(map[string]any)["failure"].(map[string]any)
	if failure["code"] != "legacy_archive" {
		t.Fatalf("legacy = %#v", old)
	}
}

func galleryZip(t *testing.T, metadata, bright bool) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	entries := map[string]string{"001.jpg": "image", "001.jpg.json": `{"width":100,"height":200,"cookie":"secret"}`}
	if metadata {
		entries["metadata.json"] = `{"source_url":"https://instagram.com/p/x/","extractor":"instagram","post_id":"x","post_url":"https://instagram.com/p/x/","author":"user","description":"caption","likes":4,"files":[{"name":"001.jpg","size":5,"content_type":"image/jpeg","width":100,"height":200}]}`
	}
	if bright {
		delete(entries, "001.jpg.json")
		entries["brightdata.json"] = `{"post_id":"x","authorization":"secret"}`
	}
	for name, value := range entries {
		w, _ := z.Create(name)
		_, _ = w.Write([]byte(value))
	}
	_ = z.Close()
	return b.Bytes()
}

func seedGalleryResult(t *testing.T, db *gorm.DB, store storage.Storage, id string, metadata, bright bool) {
	createVideoCapture(t, db, id, "https://www.instagram.com/p/"+id+"/", map[string]string{"gallery-dl": "completed"})
	key := id + "/gallery.zip"
	data := galleryZip(t, metadata, bright)
	storeTestObject(t, store, key, data)
	source := "native"
	if bright {
		source = "brightdata"
	}
	db.Model(&models.ArchiveItem{}).Where("capture_id = (SELECT id FROM captures WHERE short_id = ?)", id).Updates(map[string]any{"storage_key": key, "file_size": len(data), "source": source})
}

func TestApiArchiveResultGalleryRawPartialAliasAndSkipped(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "gal01", true, false)
	_, body := getResult(t, resultRouter(db, store), "gal01")
	social := body["social_post"].(map[string]any)
	if social["status"] != "fulfilled" || social["bundle_url"] == nil {
		t.Fatalf("gallery = %#v", social)
	}
	media := social["media"].([]any)[0].(map[string]any)
	if media["width"] != float64(100) || !strings.Contains(media["url"].(string), "/gallery/gal01/file/") {
		t.Fatalf("media = %#v", media)
	}
	rec := httptest.NewRecorder()
	resultRouter(db, store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gallery/gal01/raw", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"cookie":"secret"`) || !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("raw status=%d body=%s", rec.Code, rec.Body.String())
	}
	seedGalleryResult(t, db, store, "part1", false, false)
	_, partial := getResult(t, resultRouter(db, store), "part1")
	if partial["social_post"].(map[string]any)["status"] != "partial" {
		t.Fatalf("partial = %#v", partial)
	}
	var canonical models.Capture
	db.Where("short_id = ?", "gal01").First(&canonical)
	var archived models.ArchivedURL
	db.Where("original = ?", "https://www.instagram.com/p/gal01/").First(&archived)
	db.Create(&models.Capture{ArchivedURLID: archived.ID, Timestamp: time.Now(), ShortID: "alias", AliasOfID: &canonical.ID})
	_, alias := getResult(t, resultRouter(db, store), "alias")
	if alias["short_id"] != "alias" || alias["canonical_short_id"] != "gal01" {
		t.Fatalf("alias = %#v", alias)
	}
	createVideoCapture(t, db, "skip1", "https://www.instagram.com/p/skipped/", map[string]string{"mhtml": "completed"})
	_, skipped := getResult(t, resultRouter(db, store), "skip1")
	f := skipped["social_post"].(map[string]any)["failure"].(map[string]any)
	if f["code"] != "authentication_required" {
		t.Fatalf("skipped = %#v", skipped)
	}
}
