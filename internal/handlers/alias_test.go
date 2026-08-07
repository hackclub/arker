package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/models"
)

func newAliasTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedAliasPair(t *testing.T, db *gorm.DB) (canonical, alias models.Capture) {
	t.Helper()
	u := models.ArchivedURL{Original: "https://example.com/page"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	canonical = models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: "canon"}
	if err := db.Create(&canonical).Error; err != nil {
		t.Fatalf("create canonical: %v", err)
	}
	alias = models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: "alias", AliasOfID: &canonical.ID}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatalf("create alias: %v", err)
	}
	return canonical, alias
}

func performAliasRequest(db *gorm.DB, path string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler := func(c *gin.Context) {
		if redirectIfAlias(c, db, c.Param("shortid")) {
			return
		}
		c.String(http.StatusOK, "handled")
	}
	r.GET("/:shortid", handler)
	r.GET("/:shortid/:type", handler)
	r.GET("/archive/:shortid/:type", handler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	return w
}

func TestRedirectIfAliasRedirectsToCanonical(t *testing.T) {
	db := newAliasTestDB(t)
	seedAliasPair(t, db)

	cases := map[string]string{
		"/alias":               "/canon",
		"/alias/screenshot":    "/canon/screenshot",
		"/archive/alias/mhtml": "/archive/canon/mhtml",
		"/alias?foo=bar":       "/canon?foo=bar",
	}
	for path, want := range cases {
		w := performAliasRequest(db, path)
		if w.Code != http.StatusFound {
			t.Errorf("%s: status = %d, want 302", path, w.Code)
			continue
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("%s: Location = %q, want %q", path, got, want)
		}
	}
}

func TestRedirectIfAliasPassesThroughCanonical(t *testing.T) {
	db := newAliasTestDB(t)
	seedAliasPair(t, db)

	w := performAliasRequest(db, "/canon")
	if w.Code != http.StatusOK || w.Body.String() != "handled" {
		t.Errorf("canonical short ID must not redirect; status = %d body = %q", w.Code, w.Body.String())
	}
}

func TestRedirectIfAliasPassesThroughUnknown(t *testing.T) {
	db := newAliasTestDB(t)

	w := performAliasRequest(db, "/nope1")
	if w.Code != http.StatusOK || w.Body.String() != "handled" {
		t.Errorf("unknown short ID must fall through to the handler; status = %d body = %q", w.Code, w.Body.String())
	}
}

func TestReplaceShortIDSegmentOnlyTouchesMatchingSegment(t *testing.T) {
	got := replaceShortIDSegment("/itch/abc12/file/abc12/index.html", "abc12", "canon")
	want := "/itch/canon/file/abc12/index.html"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
