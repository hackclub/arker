package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/canary"
	"arker/internal/models"
)

// fakeCanaryService stands in for the runner so handler tests never touch an
// archiver, a probe URL, or the network.
type fakeCanaryService struct {
	cfg        canary.Config
	probes     []canary.Probe
	problems   []error
	inProgress bool
	result     canary.SweepResult
	err        error

	mu    sync.Mutex
	calls []canary.SweepOptions
}

func (f *fakeCanaryService) Config() canary.Config             { return f.cfg }
func (f *fakeCanaryService) Probes() ([]canary.Probe, []error) { return f.probes, f.problems }
func (f *fakeCanaryService) InProgress() bool                  { return f.inProgress }
func (f *fakeCanaryService) RunSweep(_ context.Context, opts canary.SweepOptions) (canary.SweepResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, opts)
	f.mu.Unlock()
	return f.result, f.err
}

func (f *fakeCanaryService) sweepCalls() []canary.SweepOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]canary.SweepOptions(nil), f.calls...)
}

func newCanaryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.CanaryRun{}, &models.BrightDataUsage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newCanaryRouter(db *gorm.DB, svc CanaryService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Same registration as cmd/main.go's admin group, minus the session guard.
	r.GET("/admin/canaries", func(c *gin.Context) { CanariesGet(c, db, svc) })
	r.POST("/admin/canaries/run", func(c *gin.Context) { CanariesRun(c, db, svc) })
	return r
}

func seedCanaryRun(t *testing.T, db *gorm.DB, key, platform, postType string, passed bool, startedAt time.Time, stage, reason string) {
	t.Helper()
	finished := startedAt.Add(time.Minute)
	run := models.CanaryRun{
		ProbeKey: key, Platform: platform, PostType: postType,
		URL: "https://example.com/" + key, Trigger: canary.TriggerSchedule,
		StartedAt: startedAt, FinishedAt: &finished, Passed: passed,
		FailureStage: stage, FailureReason: reason, ShortID: "sh" + postType,
		Provenance: models.ArchiveSourceNative,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("seed canary run: %v", err)
	}
}

func TestCanariesGetReportsHealthAndConfiguration(t *testing.T) {
	db := newCanaryTestDB(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedCanaryRun(t, db, "youtube/video", "youtube", "video", true, base, "", "")
	seedCanaryRun(t, db, "imgur/album", "imgur", "album", false, base, "media", "gallery bundle contains no image or video files")

	svc := &fakeCanaryService{
		cfg:    canary.Config{Schedule: "6h", Interval: 6 * time.Hour, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
		probes: []canary.Probe{{Platform: "youtube", PostType: "video", URL: "https://youtu.be/x", ExpectedType: "yt-dlp"}},
	}
	w := httptest.NewRecorder()
	newCanaryRouter(db, svc).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/canaries", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Schedule struct {
			Enabled bool   `json:"enabled"`
			Value   string `json:"value"`
			EnvVar  string `json:"env_var"`
		} `json:"schedule"`
		PaidFallback struct {
			Allowed       bool    `json:"allowed"`
			SpentTodayUSD float64 `json:"spent_today_usd"`
		} `json:"paid_fallback"`
		Summary canary.Summary      `json:"summary"`
		Health  []canary.SlotHealth `json:"health"`
		Probes  []struct {
			Key            string `json:"key"`
			URLOverrideEnv string `json:"url_override_env"`
		} `json:"probes"`
		Recent []models.CanaryRun `json:"recent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Schedule.Enabled || body.Schedule.Value != "6h" || body.Schedule.EnvVar != canary.ScheduleEnvVar {
		t.Errorf("schedule block = %+v, want enabled 6h", body.Schedule)
	}
	if body.PaidFallback.Allowed {
		t.Error("paid fallback reported as allowed when it is off")
	}
	if body.Summary.Status != canary.HealthFailing {
		t.Errorf("summary status = %s, want failing", body.Summary.Status)
	}
	if len(body.Summary.FailingKeys) != 1 || body.Summary.FailingKeys[0] != "imgur/album" {
		t.Errorf("failing keys = %v, want [imgur/album]", body.Summary.FailingKeys)
	}
	if len(body.Health) != 2 {
		t.Errorf("health has %d slots, want 2", len(body.Health))
	}
	if len(body.Recent) != 2 {
		t.Errorf("recent has %d runs, want 2", len(body.Recent))
	}
	if len(body.Probes) != 1 || body.Probes[0].URLOverrideEnv != canary.ProbeURLEnvPrefix+"YOUTUBE_VIDEO" {
		t.Errorf("probe view = %+v, want the rotation env var surfaced", body.Probes)
	}
}

// A server with canaries that have never run must not look healthy.
func TestCanariesGetWithNoHistoryIsUnknown(t *testing.T) {
	db := newCanaryTestDB(t)
	svc := &fakeCanaryService{cfg: canary.Config{}}
	w := httptest.NewRecorder()
	newCanaryRouter(db, svc).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/canaries", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Summary  canary.Summary `json:"summary"`
		Schedule struct {
			Enabled bool `json:"enabled"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Summary.Status != canary.HealthUnknown {
		t.Errorf("status = %s, want unknown", body.Summary.Status)
	}
	if body.Schedule.Enabled {
		t.Error("schedule reported enabled with no configuration")
	}
}

func TestCanariesRunDetachesByDefault(t *testing.T) {
	db := newCanaryTestDB(t)
	svc := &fakeCanaryService{
		probes: []canary.Probe{
			{Platform: "youtube", PostType: "video"},
			{Platform: "imgur", PostType: "album"},
		},
	}
	w := httptest.NewRecorder()
	newCanaryRouter(db, svc).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/canaries/run?platform=youtube", nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var body struct {
		Status   string   `json:"status"`
		Platform string   `json:"platform"`
		Probes   []string `json:"probes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "started" || body.Platform != "youtube" {
		t.Errorf("body = %+v, want a started youtube sweep", body)
	}
	if len(body.Probes) != 1 || body.Probes[0] != "youtube/video" {
		t.Errorf("probes = %v, want [youtube/video]", body.Probes)
	}

	// The sweep is detached, so give the goroutine a moment before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(svc.sweepCalls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := svc.sweepCalls()
	if len(calls) != 1 {
		t.Fatalf("sweep called %d times, want 1", len(calls))
	}
	if calls[0].Trigger != canary.TriggerManual || calls[0].Platform != "youtube" {
		t.Errorf("sweep options = %+v, want a manual youtube sweep", calls[0])
	}
}

func TestCanariesRunWaitReturnsResults(t *testing.T) {
	db := newCanaryTestDB(t)
	svc := &fakeCanaryService{result: canary.SweepResult{
		Trigger: canary.TriggerManual, Passed: 2, Failed: 1,
		Results: []canary.RunResult{
			{ProbeKey: "youtube/video", Passed: true},
			{ProbeKey: "imgur/album", Passed: false, Failure: "media", Reason: "no images"},
		},
	}}
	w := httptest.NewRecorder()
	newCanaryRouter(db, svc).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/canaries/run?wait=1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body canary.SweepResult
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Passed != 2 || body.Failed != 1 || len(body.Results) != 2 {
		t.Errorf("body = %+v, want the sweep verdicts", body)
	}
	if calls := svc.sweepCalls(); len(calls) != 1 || calls[0].Trigger != canary.TriggerManual {
		t.Errorf("sweep calls = %+v, want one manual sweep", calls)
	}
}

func TestCanariesRunRejectsOverlappingSweep(t *testing.T) {
	db := newCanaryTestDB(t)
	svc := &fakeCanaryService{inProgress: true}
	w := httptest.NewRecorder()
	newCanaryRouter(db, svc).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/canaries/run", nil))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if calls := svc.sweepCalls(); len(calls) != 0 {
		t.Errorf("a second sweep was started anyway: %+v", calls)
	}
}

func TestCanaryHandlersWithoutServiceAreUnavailable(t *testing.T) {
	db := newCanaryTestDB(t)
	router := newCanaryRouter(db, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/canaries"},
		{http.MethodPost, "/admin/canaries/run"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, w.Code)
		}
	}
}
