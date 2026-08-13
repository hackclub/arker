package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/canary"
)

func newHealthRouter(db *gorm.DB, provider CanaryHealthProvider) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/health", HealthCheckHandler(db, provider))
	return r
}

func getHealth(t *testing.T, router *gin.Engine) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health body %q: %v", w.Body.String(), err)
	}
	return w.Code, body
}

// Without a provider the payload is byte-for-byte what it was before canaries
// existed. Existing monitors must not notice this change.
func TestHealthWithoutCanaryProviderIsUnchanged(t *testing.T) {
	db := newCanaryTestDB(t)
	code, body := getHealth(t, newHealthRouter(db, nil))

	if code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("status = %d body = %v, want 200 healthy", code, body)
	}
	if len(body) != 1 {
		t.Errorf("payload = %v, want only the status field", body)
	}
}

func TestHealthReportsCanaryDegradation(t *testing.T) {
	db := newCanaryTestDB(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedCanaryRun(t, db, "youtube/video", "youtube", "video", true, base, "", "")
	seedCanaryRun(t, db, "reddit/gallery", "reddit", "gallery", false, base, "media", "gallery bundle contains no image or video files")

	cfg := canary.Config{Schedule: "6h", Interval: 6 * time.Hour}
	code, body := getHealth(t, newHealthRouter(db, func() canary.Summary { return canary.HealthSummary(db, cfg) }))

	// The status code must not change: /health is the liveness probe, and a
	// platform-side breakage must not restart the container.
	if code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 even while degraded", code)
	}
	if body["status"] != "healthy" {
		t.Errorf("status = %v, want healthy (degradation is reported additively)", body["status"])
	}
	if body["degraded"] != true {
		t.Errorf("degraded = %v, want true", body["degraded"])
	}
	canaries, ok := body["canaries"].(map[string]any)
	if !ok {
		t.Fatalf("canaries field = %v, want an object", body["canaries"])
	}
	if canaries["status"] != canary.HealthFailing {
		t.Errorf("canary status = %v, want failing", canaries["status"])
	}
	failing, _ := canaries["failing_probes"].([]any)
	if len(failing) != 1 || failing[0] != "reddit/gallery" {
		t.Errorf("failing probes = %v, want [reddit/gallery]", failing)
	}
	if canaries["schedule_enabled"] != true {
		t.Errorf("schedule_enabled = %v, want true", canaries["schedule_enabled"])
	}
}

func TestHealthReportsPassingCanariesWithoutDegradedFlag(t *testing.T) {
	db := newCanaryTestDB(t)
	seedCanaryRun(t, db, "youtube/video", "youtube", "video", true, time.Now().UTC(), "", "")

	cfg := canary.Config{Schedule: "6h", Interval: 6 * time.Hour}
	code, body := getHealth(t, newHealthRouter(db, func() canary.Summary { return canary.HealthSummary(db, cfg) }))

	if code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", code)
	}
	if _, present := body["degraded"]; present {
		t.Error("degraded flag is present while every canary passes")
	}
	canaries := body["canaries"].(map[string]any)
	if canaries["status"] != canary.HealthPassing {
		t.Errorf("canary status = %v, want passing", canaries["status"])
	}
}

// A server that has never run a canary reports unknown, not healthy.
func TestHealthReportsUnknownCanariesWhenDisabled(t *testing.T) {
	db := newCanaryTestDB(t)
	code, body := getHealth(t, newHealthRouter(db, func() canary.Summary { return canary.HealthSummary(db, canary.Config{}) }))

	if code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", code)
	}
	if _, present := body["degraded"]; present {
		t.Error("degraded flag set with no canary results at all")
	}
	canaries := body["canaries"].(map[string]any)
	if canaries["status"] != canary.HealthUnknown {
		t.Errorf("canary status = %v, want unknown", canaries["status"])
	}
	if canaries["schedule_enabled"] != false {
		t.Errorf("schedule_enabled = %v, want false", canaries["schedule_enabled"])
	}
}
