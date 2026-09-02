package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/workers"
)

func newThumbTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newThumbRouter(db *gorm.DB, store storage.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Same registration as cmd/main.go, including the wildcard type route.
	h := func(c *gin.Context) { ServeThumbnail(c, store, db, nil) }
	r.GET("/thumb/:shortid", h)
	r.HEAD("/thumb/:shortid", h)
	r.GET("/thumb/:shortid/*type", h)
	r.HEAD("/thumb/:shortid/*type", h)
	return r
}

func banded(w, h int, top, bottom color.RGBA) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := top
		if y >= h/2 {
			c = bottom
		}
		for x := 0; x < w; x++ {
			m.Set(x, y, c)
		}
	}
	return m
}

// seedCapture creates a capture and its items, generating and storing a real
// thumbnail through the production write path for any item marked wantThumb.
func seedCapture(t *testing.T, db *gorm.DB, store storage.Storage, shortID, original string, items []seedSpec) {
	t.Helper()
	u := models.ArchivedURL{Original: original}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	for _, spec := range items {
		item := models.ArchiveItem{
			CaptureID:       capture.ID,
			Type:            spec.typ,
			Status:          spec.status,
			StorageKey:      shortID + "/" + spec.typ + "-src",
			ThumbnailStatus: spec.thumbStatus,
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("create item: %v", err)
		}
		if spec.thumbColor == nil {
			continue
		}
		th, err := thumbnail.FromImage(banded(1200, 2400, *spec.thumbColor, color.RGBA{0, 0, 0, 255}), thumbnail.CropTop)
		if err != nil {
			t.Fatalf("build thumbnail: %v", err)
		}
		key := shortID + "/" + spec.typ + "-abcd1234-thumb.jpg"
		if err := workers.StoreThumbnail(&archivers.Thumbnail{Data: th.Data, Width: th.Width, Height: th.Height}, key, store, db, &item); err != nil {
			t.Fatalf("store thumbnail: %v", err)
		}
	}
}

type seedSpec struct {
	typ         string
	status      string
	thumbStatus string
	thumbColor  *color.RGBA
}

func dominantColor(t *testing.T, body []byte) (r, g, b int) {
	t.Helper()
	img, err := jpeg.Decode(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("response is not decodable jpeg: %v", err)
	}
	bounds := img.Bounds()
	var sr, sg, sb, n int
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += int(cr >> 8)
			sg += int(cg >> 8)
			sb += int(cb >> 8)
			n++
		}
	}
	return sr / n, sg / n, sb / n
}

// The full path: generate a thumbnail, store it the way the worker does, and
// serve it over HTTP.
func TestServeThumbnailReturnsStoredImage(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	green := color.RGBA{20, 200, 20, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &green},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/abc12", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != thumbnail.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, thumbnail.ContentType)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control = %q, want the mutable short-ID alias to revalidate", cc)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("missing ETag")
	}

	cfg, _, err := image.DecodeConfig(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width != thumbnail.Width || cfg.Height != thumbnail.Height {
		t.Errorf("served %dx%d, want %dx%d", cfg.Width, cfg.Height, thumbnail.Width, thumbnail.Height)
	}
	if _, g, _ := dominantColor(t, w.Body.Bytes()); g < 150 {
		t.Errorf("served image is not the expected thumbnail (g=%d)", g)
	}
}

func TestServeThumbnailRetainsOriginalSocialFormatAndDimensions(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	seedCapture(t, db, store, "soc01", "https://www.instagram.com/p/example/", []seedSpec{
		{typ: "gallery-dl", status: "completed"},
	})

	var source bytes.Buffer
	if err := png.Encode(&source, banded(137, 251, color.RGBA{200, 20, 20, 255}, color.RGBA{20, 20, 200, 255})); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	var item models.ArchiveItem
	if err := db.Where("type = ?", "gallery-dl").First(&item).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}
	key := "soc01/gallery-dl-abcd1234-thumb.png"
	if err := workers.StoreThumbnail(&archivers.Thumbnail{Data: source.Bytes(), Width: 137, Height: 251}, key, store, db, &item); err != nil {
		t.Fatalf("store thumbnail: %v", err)
	}

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/soc01", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", contentType)
	}
	if !bytes.Equal(w.Body.Bytes(), source.Bytes()) {
		t.Error("served social thumbnail differs from the stored provider image")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if cfg.Width != 137 || cfg.Height != 251 {
		t.Errorf("served dimensions = %dx%d, want 137x251", cfg.Width, cfg.Height)
	}
}

// Caches and link-preview crawlers probe with HEAD before fetching.
func TestServeThumbnailSupportsHEAD(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	green := color.RGBA{20, 200, 20, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &green},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("HEAD", "/thumb/abc12", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != thumbnail.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, thumbnail.ContentType)
	}
	if w.Header().Get("Content-Length") == "" {
		t.Error("missing Content-Length, which is the main thing a HEAD caller wants")
	}
}

func TestServeThumbnailSendsContentLength(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	green := color.RGBA{20, 200, 20, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &green},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/abc12", nil))

	if got, want := w.Header().Get("Content-Length"), strconv.Itoa(w.Body.Len()); got != want {
		t.Errorf("Content-Length = %q, want %q (actual body size)", got, want)
	}
}

func TestServeThumbnailResolvesAliasWithoutRedirect(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	green := color.RGBA{20, 200, 20, 255}
	seedCapture(t, db, store, "canon", "https://example.com/alias", []seedSpec{{typ: "screenshot", status: "completed", thumbColor: &green}})
	var canonical models.Capture
	if err := db.Where("short_id = ?", "canon").First(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	alias := models.Capture{ArchivedURLID: canonical.ArchivedURLID, Timestamp: time.Now(), ShortID: "alias", AliasOfID: &canonical.ID}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/alias", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("alias thumbnail status/location = %d/%q, want direct 200", rec.Code, rec.Header().Get("Location"))
	}
}

func TestServeThumbnailHonoursIfNoneMatch(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	blue := color.RGBA{20, 20, 200, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &blue},
	})
	router := newThumbRouter(db, store)

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest("GET", "/thumb/abc12", nil))
	etag := first.Header().Get("ETag")

	req := httptest.NewRequest("GET", "/thumb/abc12", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 returned %d bytes, want empty", second.Body.Len())
	}
}

func TestServeThumbnailStableURLAlwaysRevalidates(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	blue := color.RGBA{20, 20, 200, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &blue},
	})
	var item models.ArchiveItem
	if err := db.Where("type = ?", "screenshot").First(&item).Error; err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	path := "/thumb/abc12?v=" + thumbnailVersionToken(item.ThumbnailKey)
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control = %q, want revalidation even for a legacy token", got)
	}
}

// Every request returns an image. A broken <img> across a list of hundreds of
// rows is worse than a neutral tile.
func TestServeThumbnailFallsBackToPlaceholder(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed"}, // completed, but no thumbnail yet
	})
	router := newThumbRouter(db, store)

	for _, tc := range []struct{ name, path string }{{"not generated yet", "/thumb/abc12"}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
				t.Errorf("Content-Type = %q, want an SVG placeholder", ct)
			}
			// Short max-age so the real thumbnail is picked up on refresh.
			if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=60") {
				t.Errorf("Cache-Control = %q, want a short max-age", cc)
			}
			if !strings.Contains(w.Body.String(), "<svg") {
				t.Error("body is not an SVG")
			}
		})
	}
	unknown := httptest.NewRecorder()
	router.ServeHTTP(unknown, httptest.NewRequest("GET", "/thumb/zzzz9", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown capture status = %d, want 404", unknown.Code)
	}
}

func TestServeThumbnailPlaceholderEscapesHostname(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	seedCapture(t, db, store, "abc12", "https://ex<script>ample.com/page", []seedSpec{
		{typ: "screenshot", status: "pending"},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/abc12", nil))

	if strings.Contains(w.Body.String(), "<script>") {
		t.Errorf("placeholder embedded unescaped markup from the archived URL: %s", w.Body.String())
	}
}

// The preview should match the tab a visitor lands on, so it follows the same
// preference order the viewer uses.
func TestServeThumbnailPrefersViewerDefaultType(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	red := color.RGBA{200, 20, 20, 255}
	blue := color.RGBA{20, 20, 200, 255}
	// For a plain URL the viewer prefers mhtml, then screenshot.
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &blue},
		{typ: "mhtml", status: "completed", thumbColor: &red},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/abc12", nil))

	r, _, b := dominantColor(t, w.Body.Bytes())
	if r < 150 || b > 100 {
		t.Errorf("served the wrong item's thumbnail: avg r=%d b=%d, want the preferred (mhtml/red) one", r, b)
	}
}

func TestServeThumbnailPrefersSocialPosterOverPageScreenshot(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	posterRed := color.RGBA{200, 20, 20, 255}
	pageBlue := color.RGBA{20, 20, 200, 255}
	// A social capture has both a browser screenshot and its media item's own
	// poster. The latter must represent the capture even when the screenshot row
	// was created first.
	seedCapture(t, db, store, "soc02", "https://www.instagram.com/p/example/", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &pageBlue},
		{typ: "gallery-dl", status: "completed", thumbColor: &posterRed},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/soc02", nil))

	r, _, b := dominantColor(t, w.Body.Bytes())
	if r < 150 || b > 100 {
		t.Errorf("served page screenshot instead of social poster: avg r=%d b=%d", r, b)
	}
}

func TestServeThumbnailServesRequestedType(t *testing.T) {
	db := newThumbTestDB(t)
	store := storage.NewMemoryStorage()
	red := color.RGBA{200, 20, 20, 255}
	blue := color.RGBA{20, 20, 200, 255}
	seedCapture(t, db, store, "abc12", "https://example.com/page", []seedSpec{
		{typ: "screenshot", status: "completed", thumbColor: &blue},
		{typ: "mhtml", status: "completed", thumbColor: &red},
	})

	w := httptest.NewRecorder()
	newThumbRouter(db, store).ServeHTTP(w, httptest.NewRequest("GET", "/thumb/abc12/screenshot", nil))

	r, _, b := dominantColor(t, w.Body.Bytes())
	if b < 150 || r > 100 {
		t.Errorf("explicit type ignored: avg r=%d b=%d, want the screenshot (blue) one", r, b)
	}
}

func TestSelectThumbnailItem(t *testing.T) {
	preference := []string{"mhtml", "screenshot", "git"}

	t.Run("picks a completed screenshot as the generation candidate", func(t *testing.T) {
		items := []models.ArchiveItem{
			{Type: "mhtml", Status: "completed", StorageKey: "k1"},
			{Type: "screenshot", Status: "completed", StorageKey: "k2"},
		}
		ready, candidate := selectThumbnailItem(items, preference)
		if ready != nil {
			t.Errorf("ready = %+v, want nil", ready)
		}
		if candidate == nil || candidate.Type != "screenshot" {
			t.Fatalf("candidate = %+v, want the screenshot item (mhtml cannot be thumbnailed)", candidate)
		}
	})

	t.Run("never re-queues an item marked unavailable", func(t *testing.T) {
		items := []models.ArchiveItem{
			{Type: "screenshot", Status: "completed", StorageKey: "k", ThumbnailStatus: models.ThumbnailStatusUnavailable},
		}
		if _, candidate := selectThumbnailItem(items, preference); candidate != nil {
			t.Errorf("candidate = %+v, want nil for a permanently unavailable item", candidate)
		}
	})

	t.Run("ignores incomplete items", func(t *testing.T) {
		items := []models.ArchiveItem{{Type: "screenshot", Status: "processing"}}
		if _, candidate := selectThumbnailItem(items, preference); candidate != nil {
			t.Errorf("candidate = %+v, want nil while the archive is still running", candidate)
		}
	})

	t.Run("a ready thumbnail wins over a candidate", func(t *testing.T) {
		items := []models.ArchiveItem{
			{Type: "screenshot", Status: "completed", StorageKey: "k", ThumbnailKey: "t", ThumbnailStatus: models.ThumbnailStatusReady},
		}
		ready, candidate := selectThumbnailItem(items, preference)
		if ready == nil {
			t.Fatal("ready = nil, want the item")
		}
		if candidate != nil {
			t.Errorf("candidate = %+v, want nil when a thumbnail already exists", candidate)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		ready, candidate := selectThumbnailItem(nil, preference)
		if ready != nil || candidate != nil {
			t.Errorf("want (nil, nil), got (%+v, %+v)", ready, candidate)
		}
	})
}

func TestThumbnailURLIsAbsolute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/past-archives", nil)
	c.Request.Host = "archive.example.com"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	if got, want := ThumbnailURL(c, "abc12"), "https://archive.example.com/thumb/abc12"; got != want {
		t.Errorf("ThumbnailURL = %q, want %q", got, want)
	}
	if got, want := ThumbnailURL(c, "abc12", "abc12/new-thumb.jpg"), "https://archive.example.com/thumb/abc12"; got != want {
		t.Errorf("ready ThumbnailURL = %q, want stable URL %q", got, want)
	}

	// Production's internal proxy hop can say http even though the public
	// archive origin is permanently HTTPS.
	c.Request.Host = "archive.hackclub.com"
	c.Request.Header.Set("X-Forwarded-Proto", "http")
	if got, want := ThumbnailURL(c, "abc12"), "https://archive.hackclub.com/thumb/abc12"; got != want {
		t.Errorf("production ThumbnailURL = %q, want %q", got, want)
	}
}
