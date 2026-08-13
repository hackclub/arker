package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/models"
)

func newFindOrCreateHandlerTest(t *testing.T) (*gin.Engine, *gorm.DB, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}); err != nil {
		t.Fatal(err)
	}
	key, hash, err := GenerateAPIKey("test", "client", "dev")
	if err != nil {
		t.Fatal(err)
	}
	apiKey := models.APIKey{Username: "test", AppName: "client", Environment: "dev", KeyHash: hash, KeyPrefix: "test_client_dev", IsActive: true}
	if err := db.Create(&apiKey).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/api/v1/archive/find-or-create", RequireAPIKey(db), func(c *gin.Context) {
		ApiFindOrCreateArchive(c, db, nil)
	})
	return r, db, key
}

func TestApiFindOrCreateAuthenticationAndFoundResponse(t *testing.T) {
	r, db, key := newFindOrCreateHandlerTest(t)

	unauthorized := httptest.NewRecorder()
	r.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/archive/find-or-create", strings.NewReader(`{"url":"https://example.com"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	u := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&u)
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now().Add(-48 * time.Hour), ShortID: "found"}
	db.Create(&capture)
	db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "completed"})
	db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: "screenshot", Status: "completed"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive/find-or-create", strings.NewReader(`{"url":"https://example.com"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["action"] != "found" || body["short_id"] != "found" || body["status"] != "completed" {
		t.Fatalf("body = %#v", body)
	}
	if body["result_url"] != "http://example.com/found" || w.Header().Get("Location") != body["result_url"] {
		t.Fatalf("result/location = %q / %q", body["result_url"], w.Header().Get("Location"))
	}
}

func TestApiFindOrCreateValidation(t *testing.T) {
	r, _, key := newFindOrCreateHandlerTest(t)
	for _, body := range []string{`{`, `{"url":"not-a-url"}`, `{"url":"https://example.com","types":["bogus"]}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/archive/find-or-create", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, response = %s", body, w.Code, w.Body.String())
		}
	}
}
