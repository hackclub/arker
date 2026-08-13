package canary

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/models"
)

func seedRun(t *testing.T, db *gorm.DB, key string, passed bool, startedAt time.Time, failureStage string) models.CanaryRun {
	t.Helper()
	platform, postType := key[:len(key)-len(postTypeOf(key))-1], postTypeOf(key)
	finished := startedAt.Add(time.Minute)
	run := models.CanaryRun{
		ProbeKey: key, Platform: platform, PostType: postType,
		URL: "https://example.com/" + key, Trigger: TriggerSchedule,
		StartedAt: startedAt, FinishedAt: &finished, Passed: passed,
		StageReached: StagePassed, FailureStage: failureStage,
		Provenance: models.ArchiveSourceNative,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run
}

func postTypeOf(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			return key[i+1:]
		}
	}
	return key
}

// The health view is the newest row per slot, not the newest row overall: a
// platform that failed an hour ago and passed ten minutes ago reads green,
// and one that passed a week ago and failed since reads red.
func TestLatestPerSlotReturnsNewestPerSlot(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	seedRun(t, db, "youtube/video", false, base, StageMedia)
	seedRun(t, db, "youtube/video", true, base.Add(time.Hour), "")
	seedRun(t, db, "reddit/gallery", true, base, "")
	seedRun(t, db, "reddit/gallery", false, base.Add(time.Hour), StageMetadata)

	health, err := CurrentHealth(db)
	if err != nil {
		t.Fatalf("CurrentHealth: %v", err)
	}
	if len(health) != 2 {
		t.Fatalf("health has %d slots, want 2: %+v", len(health), health)
	}
	byKey := map[string]SlotHealth{}
	for _, slot := range health {
		byKey[slot.ProbeKey] = slot
	}
	if byKey["youtube/video"].Status != HealthPassing {
		t.Errorf("youtube/video = %s, want passing (its newest run passed)", byKey["youtube/video"].Status)
	}
	if byKey["reddit/gallery"].Status != HealthFailing {
		t.Errorf("reddit/gallery = %s, want failing (its newest run failed)", byKey["reddit/gallery"].Status)
	}
	if byKey["reddit/gallery"].FailureStage != StageMetadata {
		t.Errorf("failure stage = %q, want %q", byKey["reddit/gallery"].FailureStage, StageMetadata)
	}
}

func TestRecentRunsNewestFirst(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Add(-3 * time.Hour)
	seedRun(t, db, "youtube/video", true, base, "")
	seedRun(t, db, "vimeo/video", true, base.Add(time.Hour), "")
	seedRun(t, db, "imgur/album", false, base.Add(2*time.Hour), StageMedia)

	runs, err := RecentRuns(db, 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].ProbeKey != "imgur/album" {
		t.Errorf("newest run = %s, want imgur/album", runs[0].ProbeKey)
	}
}

func TestSummarizeHealth(t *testing.T) {
	now := time.Now().UTC()
	passing := SlotHealth{ProbeKey: "youtube/video", Status: HealthPassing, LastRunAt: &now}
	failing := SlotHealth{ProbeKey: "imgur/album", Status: HealthFailing, LastRunAt: &now}

	t.Run("no data is unknown, not healthy", func(t *testing.T) {
		got := SummarizeHealth(nil, Config{})
		if got.Status != HealthUnknown {
			t.Errorf("status = %s, want unknown", got.Status)
		}
		if got.ScheduleOn {
			t.Error("summary claims a schedule with none configured")
		}
		if got.Note == "" {
			t.Error("unknown status should explain itself")
		}
	})

	t.Run("all passing", func(t *testing.T) {
		got := SummarizeHealth([]SlotHealth{passing}, Config{Schedule: "6h", Interval: 6 * time.Hour})
		if got.Status != HealthPassing || got.Passing != 1 || got.Failing != 0 {
			t.Errorf("summary = %+v, want passing", got)
		}
		if !got.ScheduleOn || got.ScheduleSpec != "6h" {
			t.Errorf("schedule = %v/%q, want enabled 6h", got.ScheduleOn, got.ScheduleSpec)
		}
	})

	t.Run("one failure degrades the fleet", func(t *testing.T) {
		got := SummarizeHealth([]SlotHealth{passing, failing}, Config{})
		if got.Status != HealthFailing {
			t.Errorf("status = %s, want failing", got.Status)
		}
		if len(got.FailingKeys) != 1 || got.FailingKeys[0] != "imgur/album" {
			t.Errorf("failing keys = %v, want [imgur/album]", got.FailingKeys)
		}
		if got.Passing != 1 || got.Failing != 1 {
			t.Errorf("counts = %d passing / %d failing, want 1/1", got.Passing, got.Failing)
		}
	})
}

// /health is the container's liveness probe. An unreadable canary history must
// degrade to "unknown", never take the endpoint (and the container) down.
func TestHealthSummaryToleratesMissingTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	got := HealthSummary(db, Config{})
	if got.Status != HealthUnknown {
		t.Errorf("status = %s, want unknown", got.Status)
	}
	if got.Note == "" {
		t.Error("unavailable history should say so")
	}
}

func TestSlotHealthFromRunFillsProbeKey(t *testing.T) {
	run := models.CanaryRun{Platform: "bluesky", PostType: "post", Passed: true, StartedAt: time.Now()}
	if got := SlotHealthFromRun(run); got.ProbeKey != "bluesky/post" {
		t.Errorf("probe key = %q, want bluesky/post", got.ProbeKey)
	}
}
