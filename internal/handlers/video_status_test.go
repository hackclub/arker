package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arker/internal/models"
	"arker/internal/storage"

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
		"youtube":    "failed",
	})

	r := gin.New()
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storage.NewMemoryStorage(), db) })

	req := httptest.NewRequest(http.MethodGet, "/archive/wxXDP/youtube", nil)
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
	if body.LogsURL != "/logs/wxXDP/youtube" {
		t.Fatalf("logs_url = %q", body.LogsURL)
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

func TestBackfillMissingVideoItemsDryRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	// TikTok short link capture missing its youtube item: should be backfilled.
	createVideoCapture(t, db, "08aWq", "https://vm.tiktok.com/ZNRK8UVae/", map[string]string{
		"screenshot": "completed",
		"mhtml":      "completed",
	})
	// Video capture that already has a youtube item: should be skipped.
	createVideoCapture(t, db, "RTqKX", "https://www.youtube.com/shorts/5lhvfGxbVsA", map[string]string{
		"screenshot": "completed",
		"youtube":    "completed",
	})
	// Non-video capture: should be skipped.
	createVideoCapture(t, db, "abcde", "https://example.com", map[string]string{
		"screenshot": "completed",
	})

	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("secret"))))
	r.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user_id", uint(1))
		_ = session.Save()
		c.Next()
	})
	r.POST("/admin/backfill-videos", func(c *gin.Context) { BackfillMissingVideoItems(c, db, nil) })

	req := httptest.NewRequest(http.MethodPost, "/admin/backfill-videos?dry_run=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Message  string   `json:"message"`
		ShortIDs []string `json:"short_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.ShortIDs) != 1 || body.ShortIDs[0] != "08aWq" {
		t.Fatalf("short_ids = %v, want [08aWq]", body.ShortIDs)
	}

	// Dry run must not create items.
	var count int64
	db.Model(&models.ArchiveItem{}).Where("type = 'youtube'").Count(&count)
	if count != 1 {
		t.Fatalf("youtube item count = %d, want 1 (dry run must not create items)", count)
	}
}
