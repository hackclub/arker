package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
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

// galleryFixture describes a stored gallery bundle. The zero value is a
// pre-completeness archive: one slide, Arker's metadata.json, gallery-dl's raw
// sidecar, and nothing recorded about how much of the post is there.
type galleryFixture struct {
	// omitMetadata drops Arker's metadata.json entirely (a very old bundle).
	omitMetadata bool
	bright       bool
	// files are the media entries in the ZIP. Empty means one slide.
	files []string
	// completeness is the raw JSON for metadata.json's completeness block.
	// Empty omits the block, which is what archives written before this
	// existed look like.
	completeness string
	// column is the value written to archive_items.completeness. Empty is a
	// legacy row.
	column string
}

func (f galleryFixture) mediaNames() []string {
	if len(f.files) > 0 {
		return f.files
	}
	return []string{"001.jpg"}
}

// completeGalleryFixture is a whole 2-slide carousel: both slides stored, both
// records agreeing that nothing is missing.
func completeGalleryFixture() galleryFixture {
	return galleryFixture{
		files:        []string{"001.jpg", "002.jpg"},
		completeness: `{"state":"complete","expected":2,"stored":2}`,
		column:       "complete",
	}
}

func galleryZip(t *testing.T, f galleryFixture) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	entries := map[string]string{}
	records := make([]string, 0, len(f.mediaNames()))
	for _, name := range f.mediaNames() {
		entries[name] = "image"
		if !f.bright {
			entries[name+".json"] = `{"width":100,"height":200,"cookie":"secret"}`
		}
		records = append(records, fmt.Sprintf(`{"name":%q,"size":5,"content_type":"image/jpeg","width":100,"height":200}`, name))
	}
	if f.bright {
		entries["brightdata.json"] = `{"post_id":"x","authorization":"secret"}`
	}
	if !f.omitMetadata {
		completeness := ""
		if f.completeness != "" {
			completeness = `"completeness":` + f.completeness + `,`
		}
		entries["metadata.json"] = fmt.Sprintf(
			`{"source_url":"https://instagram.com/p/x/","extractor":"instagram","post_id":"x","post_url":"https://instagram.com/p/x/","author":"user","description":"caption","likes":4,%s"files":[%s]}`,
			completeness, strings.Join(records, ","))
	}
	for name, value := range entries {
		w, _ := z.Create(name)
		_, _ = w.Write([]byte(value))
	}
	_ = z.Close()
	return b.Bytes()
}

func seedGalleryResult(t *testing.T, db *gorm.DB, store storage.Storage, id string, f galleryFixture) {
	createVideoCapture(t, db, id, "https://www.instagram.com/p/"+id+"/", map[string]string{"gallery-dl": "completed"})
	key := id + "/gallery.zip"
	data := galleryZip(t, f)
	storeTestObject(t, store, key, data)
	source := "native"
	if f.bright {
		source = "brightdata"
	}
	db.Model(&models.ArchiveItem{}).Where("capture_id = (SELECT id FROM captures WHERE short_id = ?)", id).
		Updates(map[string]any{"storage_key": key, "file_size": len(data), "source": source, "completeness": f.column})
}

func socialOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	social, ok := body["social_post"].(map[string]any)
	if !ok {
		t.Fatalf("social_post missing from %#v", body)
	}
	return social
}

func warningCodes(social map[string]any) []string {
	raw, _ := social["warnings"].([]any)
	codes := make([]string, 0, len(raw))
	for _, entry := range raw {
		if warning, ok := entry.(map[string]any); ok {
			codes = append(codes, fmt.Sprint(warning["code"]))
		}
	}
	return codes
}

func hasCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

func TestApiArchiveResultGalleryRawPartialAliasAndSkipped(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "gal01", completeGalleryFixture())
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
	seedGalleryResult(t, db, store, "part1", galleryFixture{omitMetadata: true})
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

// A carousel that lost slides must never read fulfilled. This is the false
// green the completeness work exists to remove: before it, three stored slides
// of a ten-slide post produced the same response as a complete three-slide one.
func TestPartialGalleryIsNeverFulfilled(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "prt01", galleryFixture{
		files:        []string{"001.jpg", "003.jpg"},
		completeness: `{"state":"partial","expected":4,"stored":2,"missing_indices":[2,4]}`,
		column:       "partial",
	})

	_, body := getResult(t, resultRouter(db, store), "prt01")
	social := socialOf(t, body)

	if social["fulfilled"] != false || social["status"] != "partial" {
		t.Fatalf("a partial carousel read %v/%v, want fulfilled=false status=partial", social["fulfilled"], social["status"])
	}
	completeness := social["completeness"].(map[string]any)
	if completeness["state"] != "partial" || completeness["expected"] != float64(4) || completeness["stored"] != float64(2) {
		t.Fatalf("completeness = %#v", completeness)
	}
	if missing := completeness["missing_indices"].([]any); len(missing) != 2 || missing[0] != float64(2) {
		t.Fatalf("missing_indices = %#v, want [2 4]", missing)
	}
	if codes := warningCodes(social); !hasCode(codes, "media_incomplete") {
		t.Fatalf("warnings = %v, want media_incomplete", codes)
	}
	failure := social["failure"].(map[string]any)
	if failure["code"] != "media_incomplete" {
		t.Fatalf("failure = %#v, want media_incomplete", failure)
	}
	// The media that did survive still has to be listed and servable: the point
	// is to label the archive honestly, not to hide it.
	if len(social["media"].([]any)) != 2 {
		t.Fatalf("media = %#v, want both stored slides", social["media"])
	}
}

// An archive nobody can vouch for is not green either. Most extractors never
// report a file count, so "we stored everything" is unprovable and must not be
// claimed.
func TestUnknownCompletenessIsNeverFulfilled(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	// A bundle written before completeness tracking: full metadata, real media,
	// nothing saying how many assets the post has.
	seedGalleryResult(t, db, store, "unk01", galleryFixture{})

	_, body := getResult(t, resultRouter(db, store), "unk01")
	social := socialOf(t, body)

	if social["fulfilled"] != false || social["status"] != "partial" {
		t.Fatalf("unknown completeness read %v/%v, want fulfilled=false status=partial", social["fulfilled"], social["status"])
	}
	if state := social["completeness"].(map[string]any)["state"]; state != "unknown" {
		t.Fatalf("state = %v, want unknown", state)
	}
	if _, reported := social["completeness"].(map[string]any)["expected"]; reported {
		t.Fatalf("an unknown count must not report an expected value: %#v", social["completeness"])
	}
	if codes := warningCodes(social); !hasCode(codes, "completeness_unknown") {
		t.Fatalf("warnings = %v, want completeness_unknown", codes)
	}
	failure := social["failure"].(map[string]any)
	if failure["code"] != "completeness_unknown" || failure["retryable"] != false {
		t.Fatalf("failure = %#v, want a non-retryable completeness_unknown", failure)
	}
}

// The positive case: every slide stored, both records agreeing, raw provider
// metadata still retrievable. Only this reads green.
func TestCompleteCarouselIsFulfilled(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "cmp01", completeGalleryFixture())

	_, body := getResult(t, resultRouter(db, store), "cmp01")
	social := socialOf(t, body)

	if social["fulfilled"] != true || social["status"] != "fulfilled" {
		t.Fatalf("complete carousel read %v/%v, want fulfilled", social["fulfilled"], social["status"])
	}
	completeness := social["completeness"].(map[string]any)
	if completeness["state"] != "complete" || completeness["expected"] != float64(2) || completeness["stored"] != float64(2) {
		t.Fatalf("completeness = %#v", completeness)
	}
	if social["failure"] != nil {
		t.Fatalf("failure = %#v, want none", social["failure"])
	}
	if codes := warningCodes(social); len(codes) != 0 {
		t.Fatalf("warnings = %v, want none", codes)
	}
	if len(social["raw_metadata"].([]any)) == 0 {
		t.Fatal("a fulfilled archive must expose its raw provider metadata")
	}
}

// Fulfilled claims the provider's raw record is still readable. A bundle whose
// sidecars are gone keeps its media but loses the claim.
func TestFulfillmentRequiresRetrievableRawMetadata(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	fixture := completeGalleryFixture()
	fixture.bright = true
	seedGalleryResult(t, db, store, "raw01", fixture)
	// Bright Data bundles carry brightdata.json, so that one is fulfilled.
	_, withRaw := getResult(t, resultRouter(db, store), "raw01")
	if socialOf(t, withRaw)["fulfilled"] != true {
		t.Fatalf("bright data bundle = %#v", socialOf(t, withRaw))
	}

	// Now the same complete bundle with no provider record in it at all.
	db2 := newHandlerLogTestDB(t)
	store2 := storage.NewMemoryStorage()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("metadata.json")
	_, _ = w.Write([]byte(`{"source_url":"https://instagram.com/p/x/","extractor":"instagram","post_id":"x","completeness":{"state":"complete","expected":1,"stored":1},"files":[{"name":"001.jpg","content_type":"image/jpeg"}]}`))
	w, _ = z.Create("001.jpg")
	_, _ = w.Write([]byte("image"))
	_ = z.Close()
	createVideoCapture(t, db2, "raw02", "https://www.instagram.com/p/raw02/", map[string]string{"gallery-dl": "completed"})
	storeTestObject(t, store2, "raw02/gallery.zip", b.Bytes())
	db2.Model(&models.ArchiveItem{}).Where("type = ?", "gallery-dl").
		Updates(map[string]any{"storage_key": "raw02/gallery.zip", "completeness": "complete"})

	_, body := getResult(t, resultRouter(db2, store2), "raw02")
	social := socialOf(t, body)
	if social["fulfilled"] != false {
		t.Fatalf("a bundle with no raw provider record read fulfilled: %#v", social)
	}
	if codes := warningCodes(social); !hasCode(codes, "raw_metadata_unavailable") {
		t.Fatalf("warnings = %v, want raw_metadata_unavailable", codes)
	}
	if social["failure"].(map[string]any)["code"] != "raw_metadata_unavailable" {
		t.Fatalf("failure = %#v", social["failure"])
	}
}

// Existing rows have an empty completeness column, and AutoMigrate leaves them
// that way. A video captured with both sidecars is structurally one asset, so
// it stays fulfilled; a video from before sidecars existed keeps its own
// legacy_archive code rather than being relabelled.
func TestLegacyRowsAreUnaffected(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "leg01", "https://www.youtube.com/watch?v=x", map[string]string{"yt-dlp": "completed"})
	storeTestObject(t, store, "leg/meta.json", []byte(`{"schema_version":"1","platform":"youtube","post_id":"x","title":"Title","media":{"content_type":"video/mp4"},"provider":"yt-dlp"}`))
	storeTestObject(t, store, "leg/raw.json", []byte(`{"id":"x"}`))
	db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").Updates(map[string]any{
		"storage_key": "leg/media.mp4", "file_size": 8,
		"metadata_key": "leg/meta.json", "raw_metadata_key": "leg/raw.json",
	})

	var item models.ArchiveItem
	db.Where("type = ?", "yt-dlp").First(&item)
	if item.Completeness != "" {
		t.Fatalf("fixture is not a legacy row: completeness = %q", item.Completeness)
	}

	_, body := getResult(t, resultRouter(db, store), "leg01")
	social := socialOf(t, body)
	if social["fulfilled"] != true || social["status"] != "fulfilled" {
		t.Fatalf("a complete legacy video stopped reading fulfilled: %#v", social)
	}
	completeness := social["completeness"].(map[string]any)
	if completeness["state"] != "complete" || completeness["expected"] != float64(1) {
		t.Fatalf("completeness = %#v, want a structurally complete single video", completeness)
	}

	// The other kind of legacy row: no sidecars at all.
	createVideoCapture(t, db, "leg02", "https://youtu.be/older", map[string]string{"youtube": "completed"})
	db.Model(&models.ArchiveItem{}).Where("type = ?", "youtube").Update("storage_key", "leg/older.mp4")
	_, older := getResult(t, resultRouter(db, store), "leg02")
	olderSocial := socialOf(t, older)
	if olderSocial["fulfilled"] != false {
		t.Fatalf("a sidecar-less legacy video read fulfilled: %#v", olderSocial)
	}
	if olderSocial["failure"].(map[string]any)["code"] != "legacy_archive" {
		t.Fatalf("failure = %#v, want legacy_archive", olderSocial["failure"])
	}
}

// A capture can hold both extractors' items. Picking whichever row loaded first
// made the answer depend on ordering; the URL's routed type decides, and a
// completed item beats a failed one of the same type.
func TestSocialItemSelectionPrefersRoutedTypeAndCompleted(t *testing.T) {
	now := gorm.Model{UpdatedAt: time.Now()}
	older := gorm.Model{UpdatedAt: time.Now().Add(-time.Hour)}
	items := []models.ArchiveItem{
		{Model: now, Type: "mhtml", Status: "completed"},
		{Model: now, Type: "yt-dlp", Status: "failed"},
		{Model: older, Type: "gallery-dl", Status: "completed"},
	}

	// A photo post routes to gallery-dl even though a failed video item sorts
	// first in the slice.
	if got := selectSocialItem(items, "https://www.instagram.com/p/abc/"); got == nil || got.Type != "gallery-dl" {
		t.Fatalf("photo post selected %+v, want the gallery-dl item", got)
	}
	// A reel routes to yt-dlp; the routed type wins even when the other item
	// completed, because that is the capture the URL asked for.
	if got := selectSocialItem(items, "https://www.instagram.com/reel/abc/"); got == nil || got.Type != "yt-dlp" {
		t.Fatalf("reel selected %+v, want the yt-dlp item", got)
	}
	// Two rows of the same archiver (the retired "youtube" spelling beside the
	// current one): the completed one wins.
	both := []models.ArchiveItem{
		{Model: now, Type: "youtube", Status: "failed"},
		{Model: older, Type: "yt-dlp", Status: "completed"},
	}
	if got := selectSocialItem(both, "https://youtu.be/abc"); got == nil || got.Status != "completed" {
		t.Fatalf("selected %+v, want the completed row", got)
	}
	if got := selectSocialItem([]models.ArchiveItem{{Type: "mhtml", Status: "completed"}}, "https://youtu.be/abc"); got != nil {
		t.Fatalf("selected %+v from a capture with no social item, want nil", got)
	}
}

// End to end through the API: a completed gallery item must be reported even
// when a failed video item exists on the same capture.
func TestApiPrefersCompletedSocialItemOverFailedSibling(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "mix01", completeGalleryFixture())
	var capture models.Capture
	db.Where("short_id = ?", "mix01").First(&capture)
	db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: "yt-dlp", Status: "failed"})

	_, body := getResult(t, resultRouter(db, store), "mix01")
	social := socialOf(t, body)
	if social["status"] != "fulfilled" {
		t.Fatalf("status = %v, want the completed gallery item to be reported: %#v", social["status"], social)
	}
}

// Provenance has to explain a degraded archive without leaking anything: the
// attempt count, a sanitized reason, and what the paid fallback actually did.
func TestProvenanceEnrichment(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "prv01", galleryFixture{
		completeness: `{"state":"partial","expected":3,"stored":1}`,
		column:       "partial",
		bright:       true,
	})

	var item models.ArchiveItem
	db.Where("type = ?", "gallery-dl").First(&item)
	if err := utils.AppendArchiveItemLog(db, item.ID, 1,
		"Starting gallery archive\n[instagram][error] HTTP 403 for https://scontent.cdninstagram.com/v/t51.jpg?_nc_sid=SECRETSIG&oe=deadbeef\nkeeping partial archive\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	db.Create(&models.BrightDataUsage{ArchiveItemID: item.ID, ShortID: "prv01", Product: "web_scraper", DatasetID: "gd_x", SnapshotID: "s_1", Records: 1, CostUSD: 0.0015, Success: false})
	db.Create(&models.BrightDataUsage{ArchiveItemID: item.ID, ShortID: "prv01", Product: "web_scraper", DatasetID: "gd_x", SnapshotID: "s_2", Records: 3, CostUSD: 0.0045, Success: true})
	db.Create(&models.BrightDataUsage{ArchiveItemID: item.ID, ShortID: "prv01", Product: "browser_api", BytesTransferred: 1_000_000, CostUSD: 0.0084, Success: true})

	_, body := getResult(t, resultRouter(db, store), "prv01")
	social := socialOf(t, body)
	prov := social["provenance"].(map[string]any)

	if prov["source"] != "brightdata" || prov["mode"] != "fallback" {
		t.Fatalf("provenance source/mode = %#v", prov)
	}
	if prov["attempts"] != float64(3) {
		t.Fatalf("attempts = %v, want the item's retry count", prov["attempts"])
	}

	reason, _ := prov["last_failure_reason"].(string)
	if !strings.Contains(reason, "HTTP 403") {
		t.Fatalf("last_failure_reason = %q, want the last failure line", reason)
	}
	// The signed CDN URL in that line must not survive into a public response.
	if strings.Contains(reason, "http") || strings.Contains(reason, "SECRETSIG") {
		t.Fatalf("last_failure_reason leaked a URL or credential: %q", reason)
	}
	if !strings.Contains(reason, "[url]") {
		t.Fatalf("last_failure_reason = %q, want the URL replaced with a placeholder", reason)
	}

	ops := prov["fallback_ops"].([]any)
	if len(ops) != 2 {
		t.Fatalf("fallback_ops = %#v, want one entry per product", ops)
	}
	// Sorted by product, so browser_api comes first.
	browser := ops[0].(map[string]any)
	scraper := ops[1].(map[string]any)
	if browser["product"] != "browser_api" || browser["bytes_transferred"] != float64(1_000_000) {
		t.Fatalf("browser op = %#v", browser)
	}
	if scraper["product"] != "web_scraper" || scraper["operations"] != float64(2) || scraper["successes"] != float64(1) {
		t.Fatalf("scraper op = %#v", scraper)
	}
	if scraper["records"] != float64(4) {
		t.Fatalf("records = %v, want both attempts summed", scraper["records"])
	}
	// Failed operations are billable, so their cost counts too.
	if cost := scraper["estimated_cost_usd"].(float64); cost < 0.0059 || cost > 0.0061 {
		t.Fatalf("estimated_cost_usd = %v, want 0.006", cost)
	}
	snapshots := scraper["snapshot_ids"].([]any)
	if len(snapshots) != 2 || snapshots[0] != "s_1" {
		t.Fatalf("snapshot_ids = %#v", snapshots)
	}
}

// A clean, fulfilled archive must not carry an alarming failure reason, and an
// archive with no Bright Data rows must not grow an empty ops array.
func TestProvenanceStaysQuietOnAFulfilledNativeArchive(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedGalleryResult(t, db, store, "qui01", completeGalleryFixture())
	var item models.ArchiveItem
	db.Where("type = ?", "gallery-dl").First(&item)
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "Retrying after a transient error\nSuccessfully created gallery ZIP archive\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	_, body := getResult(t, resultRouter(db, store), "qui01")
	prov := socialOf(t, body)["provenance"].(map[string]any)
	if _, present := prov["last_failure_reason"]; present {
		t.Fatalf("a fulfilled archive reported a failure reason: %#v", prov)
	}
	if _, present := prov["fallback_ops"]; present {
		t.Fatalf("a native-only archive reported fallback ops: %#v", prov)
	}
}

// Provenance is most useful exactly when there is no artifact to inspect, so a
// failed item must still report attempts and the reason.
func TestProvenanceOnFailedSocialItem(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "fail2", "https://www.youtube.com/watch?v=gone", map[string]string{"yt-dlp": "failed"})
	var item models.ArchiveItem
	db.Where("type = ?", "yt-dlp").First(&item)
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "ERROR: Video unavailable: This video has been removed\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	_, body := getResult(t, resultRouter(db, store), "fail2")
	social := socialOf(t, body)
	if social["status"] != "failed" || social["completeness"] != nil {
		t.Fatalf("failed item = %#v", social)
	}
	prov := social["provenance"].(map[string]any)
	if prov["attempts"] != float64(3) {
		t.Fatalf("attempts = %v", prov["attempts"])
	}
	if reason, _ := prov["last_failure_reason"].(string); !strings.Contains(reason, "Video unavailable") {
		t.Fatalf("last_failure_reason = %q", reason)
	}
}

// Rows written before chunked logs existed keep the whole log in the item's own
// column; the reason has to come from there too.
func TestLastFailureReasonReadsLegacyLogColumn(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "lgl01", "https://youtu.be/legacylog", map[string]string{"yt-dlp": "failed"})
	db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").
		Update("logs", "starting\nERROR: Private video. Sign in if you've been granted access\n")

	_, body := getResult(t, resultRouter(db, store), "lgl01")
	prov := socialOf(t, body)["provenance"].(map[string]any)
	if reason, _ := prov["last_failure_reason"].(string); !strings.Contains(reason, "Private video") {
		t.Fatalf("last_failure_reason = %q, want the legacy column's failure line", reason)
	}
}

// Only the tail of a long log is read, and a chunk boundary must not leave a
// fragment of a word at the start of the reported reason.
func TestLastFailureReasonReadsOnlyTheLogTail(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "tail1", "https://youtu.be/longlog", map[string]string{"yt-dlp": "failed"})
	var item models.ArchiveItem
	db.Where("type = ?", "yt-dlp").First(&item)

	// An early failure that must fall out of the window, then enough verbose
	// output to push it there, then the failure that actually matters.
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "ERROR: an ancient failure nobody should see\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, strings.Repeat("[debug] routine progress chatter\n", 4000)); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "ERROR: Video unavailable\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	_, body := getResult(t, resultRouter(db, store), "tail1")
	prov := socialOf(t, body)["provenance"].(map[string]any)
	reason, _ := prov["last_failure_reason"].(string)
	if !strings.Contains(reason, "Video unavailable") {
		t.Fatalf("last_failure_reason = %q, want the most recent failure", reason)
	}
	if strings.Contains(reason, "ancient") {
		t.Fatalf("last_failure_reason = %q, want the newest failure, not the first", reason)
	}
}
