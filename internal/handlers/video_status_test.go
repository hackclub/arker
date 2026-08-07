package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"os"
	"path/filepath"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func createVideoCapture(t *testing.T, db *gorm.DB, shortID, original string, items map[string]string) models.Capture {
	t.Helper()
	url := models.ArchivedURL{Original: original}
	if err := db.Create(&url).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	capture := models.Capture{
		ArchivedURLID: url.ID,
		Timestamp:     time.Now(),
		ShortID:       shortID,
	}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	for typ, status := range items {
		item := models.ArchiveItem{
			CaptureID:  capture.ID,
			Type:       typ,
			Status:     status,
			RetryCount: 3,
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("create item %s: %v", typ, err)
		}
	}
	return capture
}

func TestServeArchiveFailedItemReturnsStatusJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	createVideoCapture(t, db, "wxXDP", "https://www.instagram.com/reel/DaPiV0zgYEr/", map[string]string{
		"screenshot": "completed",
		"yt-dlp":     "failed",
	})

	r := gin.New()
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storage.NewMemoryStorage(), db) })

	req := httptest.NewRequest(http.MethodGet, "/archive/wxXDP/yt-dlp", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error      string `json:"error"`
		Status     string `json:"status"`
		RetryCount int    `json:"retry_count"`
		LogsURL    string `json:"logs_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "failed" {
		t.Fatalf("status field = %q, want failed", body.Status)
	}
	if body.RetryCount != 3 {
		t.Fatalf("retry_count = %d, want 3", body.RetryCount)
	}
	if body.LogsURL != "/logs/wxXDP/yt-dlp" {
		t.Fatalf("logs_url = %q, want the canonical type", body.LogsURL)
	}
}

// Pages archived before the rename embed /archive/{id}/youtube in every
// <video>, <img>, and download link. Those URLs must keep resolving, in both
// directions: a legacy URL against a migrated row, and a canonical URL against
// a row the migration has not reached yet.
func TestServeArchiveResolvesLegacyAndCanonicalTypeNames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		storedType string
		requestURL string
	}{
		{"legacy URL, migrated row", "yt-dlp", "/archive/wxXDP/youtube"},
		{"canonical URL, migrated row", "yt-dlp", "/archive/wxXDP/yt-dlp"},
		{"legacy URL, un-migrated row", "youtube", "/archive/wxXDP/youtube"},
		{"canonical URL, un-migrated row", "youtube", "/archive/wxXDP/yt-dlp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newHandlerLogTestDB(t)
			createVideoCapture(t, db, "wxXDP", "https://www.instagram.com/reel/DaPiV0zgYEr/", map[string]string{
				tt.storedType: "failed",
			})

			r := gin.New()
			r.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storage.NewMemoryStorage(), db) })

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.requestURL, nil))

			var body struct {
				Error  string `json:"error"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			// The item resolved if we get its status back; "archive not found"
			// means the lookup missed the row entirely.
			if body.Status != "failed" {
				t.Fatalf("GET %s (stored as %q) = %q / status %q, want the item to resolve",
					tt.requestURL, tt.storedType, body.Error, body.Status)
			}
		})
	}
}

func TestServeArchiveMissingItemReturnsPlainNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	createVideoCapture(t, db, "08aWq", "https://vm.tiktok.com/ZNRK8UVae/", map[string]string{
		"screenshot": "completed",
	})

	r := gin.New()
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storage.NewMemoryStorage(), db) })

	req := httptest.NewRequest(http.MethodGet, "/archive/08aWq/youtube", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "" {
		t.Fatalf("status field = %q, want empty for missing item", body.Status)
	}
	if body.Error == "" {
		t.Fatal("error field is empty")
	}
}

// withMediaCookies configures a cookie jar for the test. The gallery-dl
// backfill skips login-only sites without one, so an Instagram backfill test
// has to declare that cookies exist.
func withMediaCookies(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatalf("write cookies: %v", err)
	}
	if _, err := utils.InitYtDlpCookies(path, "", dir); err != nil {
		t.Fatalf("init cookies: %v", err)
	}
	t.Cleanup(func() { utils.InitYtDlpCookies("", "", dir) })
}

func newBackfillRouter(t *testing.T, db *gorm.DB) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("secret"))))
	r.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user_id", uint(1))
		_ = session.Save()
		c.Next()
	})
	r.POST("/admin/backfill-media", func(c *gin.Context) { BackfillMissingMediaItems(c, db, nil) })
	return r
}

type backfillResponse struct {
	Message  string              `json:"message"`
	Count    int                 `json:"count"`
	ShortIDs map[string][]string `json:"short_ids"`
}

func doBackfill(t *testing.T, r *gin.Engine, query string) backfillResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/backfill-media?"+query, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body backfillResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func TestBackfillMissingMediaItemsDryRunYtDlp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	// TikTok short link capture missing its yt-dlp item: should be backfilled.
	createVideoCapture(t, db, "08aWq", "https://vm.tiktok.com/ZNRK8UVae/", map[string]string{
		"screenshot": "completed",
		"mhtml":      "completed",
	})
	// Video capture that already has a yt-dlp item: should be skipped.
	createVideoCapture(t, db, "RTqKX", "https://www.youtube.com/shorts/5lhvfGxbVsA", map[string]string{
		"screenshot": "completed",
		"yt-dlp":     "completed",
	})
	// Non-video capture: should be skipped.
	createVideoCapture(t, db, "abcde", "https://example.com", map[string]string{
		"screenshot": "completed",
	})

	body := doBackfill(t, newBackfillRouter(t, db), "type=yt-dlp&dry_run=true")

	got := body.ShortIDs[utils.ArchiveTypeYtDlp]
	if len(got) != 1 || got[0] != "08aWq" {
		t.Fatalf("short_ids[yt-dlp] = %v, want [08aWq]", got)
	}

	// Dry run must not create items.
	var count int64
	db.Model(&models.ArchiveItem{}).Where("type = ?", utils.ArchiveTypeYtDlp).Count(&count)
	if count != 1 {
		t.Fatalf("yt-dlp item count = %d, want 1 (dry run must not create items)", count)
	}
}

// Instagram feed posts archived before gallery-dl existed are the reason this
// endpoint was generalized: they have no gallery-dl item and their yt-dlp item
// (if any) could never have succeeded.
func TestBackfillMissingMediaItemsDryRunGalleryDL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withMediaCookies(t)
	db := newHandlerLogTestDB(t)
	createVideoCapture(t, db, "7fbf9", "https://www.instagram.com/p/Dbj-2q4jWvx/", map[string]string{
		"screenshot": "completed",
		"yt-dlp":     "failed",
	})
	// Already has a gallery-dl item: should be skipped.
	createVideoCapture(t, db, "2IYea", "https://www.instagram.com/p/DbkB-6DDZfu/", map[string]string{
		"screenshot": "completed",
		"gallery-dl": "completed",
	})
	// A reel is yt-dlp's job, not gallery-dl's: should be skipped.
	createVideoCapture(t, db, "RTqKX", "https://www.instagram.com/reel/DPAid-WDi67/", map[string]string{
		"screenshot": "completed",
		"yt-dlp":     "completed",
	})

	body := doBackfill(t, newBackfillRouter(t, db), "type=gallery-dl&dry_run=true")

	got := body.ShortIDs[utils.ArchiveTypeGalleryDl]
	if len(got) != 1 || got[0] != "7fbf9" {
		t.Fatalf("short_ids[gallery-dl] = %v, want [7fbf9]", got)
	}
}

// The limit is the guard against re-running a bulk Instagram backfill at the
// concurrency that previously got the account soft-blocked.
func TestBackfillMissingMediaItemsRespectsLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withMediaCookies(t)
	db := newHandlerLogTestDB(t)
	for _, shortID := range []string{"aaaaa", "bbbbb", "ccccc"} {
		createVideoCapture(t, db, shortID, "https://www.instagram.com/p/"+shortID+"/", map[string]string{
			"screenshot": "completed",
		})
	}

	body := doBackfill(t, newBackfillRouter(t, db), "type=gallery-dl&dry_run=true&limit=2")

	if body.Count != 2 {
		t.Fatalf("count = %d, want 2", body.Count)
	}
}

func TestBackfillMissingMediaItemsRejectsUnknownType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/backfill-media?type=mhtml", nil)
	rec := httptest.NewRecorder()
	newBackfillRouter(t, db).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The legacy "youtube" spelling must keep selecting the yt-dlp backfill.
func TestBackfillMissingMediaItemsAcceptsLegacyTypeName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	createVideoCapture(t, db, "08aWq", "https://vm.tiktok.com/ZNRK8UVae/", map[string]string{
		"screenshot": "completed",
	})

	body := doBackfill(t, newBackfillRouter(t, db), "type=youtube&dry_run=true")

	got := body.ShortIDs[utils.ArchiveTypeYtDlp]
	if len(got) != 1 || got[0] != "08aWq" {
		t.Fatalf("short_ids[yt-dlp] = %v, want [08aWq]", got)
	}
}

// Without cookies the Instagram backfill must select nothing: queueing
// thousands of guaranteed-failed jobs is how an unattended backfill rate-limits
// the archiver out of the captures it could have made.
func TestBackfillMissingMediaItemsSkipsLoginOnlySitesWithoutCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	createVideoCapture(t, db, "7fbf9", "https://www.instagram.com/p/Dbj-2q4jWvx/", map[string]string{
		"screenshot": "completed",
	})
	createVideoCapture(t, db, "imgr1", "https://imgur.com/a/Kn9lB", map[string]string{
		"screenshot": "completed",
	})

	body := doBackfill(t, newBackfillRouter(t, db), "type=gallery-dl&dry_run=true")

	got := body.ShortIDs[utils.ArchiveTypeGalleryDl]
	for _, shortID := range got {
		if shortID == "7fbf9" {
			t.Error("Instagram post was selected for backfill with no cookies configured")
		}
	}
	// The anonymous site is still backfilled.
	found := false
	for _, shortID := range got {
		if shortID == "imgr1" {
			found = true
		}
	}
	if !found {
		t.Errorf("short_ids = %v, want the Imgur capture, which works anonymously", got)
	}
}
