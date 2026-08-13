package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestHealthCheckReportsHealthyDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	router := gin.New()
	router.GET("/health", HealthCheckHandler(db))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if w.Code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("status = %d body = %v, want 200 healthy", w.Code, body)
	}
	if len(body) != 1 {
		t.Fatalf("payload = %v, want only the status field", body)
	}
}
