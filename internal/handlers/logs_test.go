package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arker/internal/models"
	"arker/internal/utils"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newHandlerLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate sqlite db: %v", err)
	}
	return db
}

func createHandlerLogItem(t *testing.T, db *gorm.DB) models.ArchiveItem {
	t.Helper()
	url := models.ArchivedURL{Original: "https://example.com"}
	if err := db.Create(&url).Error; err != nil {
		t.Fatalf("create url: %v", err)
	}
	capture := models.Capture{
		ArchivedURLID: url.ID,
		Timestamp:     time.Now(),
		ShortID:       "abc12",
	}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	item := models.ArchiveItem{
		CaptureID: capture.ID,
		Type:      "mhtml",
		Status:    "failed",
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
	if err := utils.AppendArchiveItemLog(db, item.ID, 1, "first\n"); err != nil {
		t.Fatalf("append first log: %v", err)
	}
	if err := utils.AppendArchiveItemLog(db, item.ID, 2, "second\n"); err != nil {
		t.Fatalf("append second log: %v", err)
	}
	return item
}

func TestGetLogsReturnsReconstructedLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	createHandlerLogItem(t, db)

	r := gin.New()
	r.GET("/logs/:shortid/:type", func(c *gin.Context) { GetLogs(c, db) })

	req := httptest.NewRequest(http.MethodGet, "/logs/abc12/web", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Logs   string `json:"logs"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Logs != "first\nsecond\n" {
		t.Fatalf("logs = %q", body.Logs)
	}
	if body.Status != "failed" {
		t.Fatalf("status = %q", body.Status)
	}
}

func TestGetLogsMissingItemReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)

	r := gin.New()
	r.GET("/logs/:shortid/:type", func(c *gin.Context) { GetLogs(c, db) })

	req := httptest.NewRequest(http.MethodGet, "/logs/missing/web", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGetItemLogReturnsReconstructedLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	item := createHandlerLogItem(t, db)

	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("secret"))))
	r.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user_id", uint(1))
		_ = session.Save()
		c.Next()
	})
	r.GET("/admin/item/:id/log", func(c *gin.Context) { GetItemLog(c, db) })

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/item/%d/log", item.ID), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Logs != "first\nsecond\n" {
		t.Fatalf("logs = %q", body.Logs)
	}
}
