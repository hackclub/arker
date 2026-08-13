package canary

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/workers"
)

func TestPeriodicJobOnlyExistsWhenScheduled(t *testing.T) {
	if job := PeriodicJob(Config{}); job != nil {
		t.Error("a periodic job exists with no schedule configured")
	}
	if job := PeriodicJob(Config{Schedule: "off"}); job != nil {
		t.Error("a periodic job exists with the schedule turned off")
	}
	if job := PeriodicJob(Config{Schedule: "6h", Interval: 6 * time.Hour}); job == nil {
		t.Error("no periodic job was created for a configured schedule")
	}
}

// The disabled-by-default guarantee, at the last line of defense: even a job
// that reaches the worker does nothing unless the schedule is configured.
func TestWorkerIgnoresScheduledSweepWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	ran := false
	runner := newTestRunner(t, db, store, Config{}, []Probe{withDefaults(videoProbe())},
		func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
			ran = true
			return errors.New("stop here")
		})

	worker := NewWorker(runner)
	job := &river.Job[JobArgs]{Args: JobArgs{Trigger: TriggerSchedule}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if ran {
		t.Error("a scheduled sweep ran while canaries were disabled")
	}
	var runs int64
	db.Model(&models.CanaryRun{}).Count(&runs)
	if runs != 0 {
		t.Errorf("recorded %d runs while disabled, want none", runs)
	}
}

// An operator asking for a sweep by hand is always honored: it is free, native,
// and how the pre-activation check in the runbook works.
func TestWorkerRunsManualSweepEvenWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	runner := newTestRunner(t, db, store, Config{}, []Probe{withDefaults(videoProbe())}, completingArchive(t, db, store))

	worker := NewWorker(runner)
	job := &river.Job[JobArgs]{Args: JobArgs{Trigger: TriggerManual}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var runs int64
	db.Model(&models.CanaryRun{}).Count(&runs)
	if runs != 1 {
		t.Errorf("recorded %d runs, want 1", runs)
	}
}

// A failing probe is data, not a job failure: returning an error would make
// River retry the sweep and re-archive every probe URL.
func TestWorkerDoesNotFailTheJobOnProbeFailure(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	runner := newTestRunner(t, db, store, Config{Schedule: "6h", Interval: 6 * time.Hour},
		[]Probe{withDefaults(videoProbe())},
		func(context.Context, workers.ArchiveJobArgs, *models.ArchiveItem, map[string]archivers.Archiver) error {
			return errors.New("extractor exploded")
		})

	worker := NewWorker(runner)
	if err := worker.Work(context.Background(), &river.Job[JobArgs]{Args: JobArgs{Trigger: TriggerSchedule}}); err != nil {
		t.Fatalf("Work returned %v; a failing probe must not fail the job", err)
	}
	var run models.CanaryRun
	if err := db.First(&run).Error; err != nil {
		t.Fatalf("no canary run recorded: %v", err)
	}
	if run.Passed {
		t.Error("failing probe recorded as a pass")
	}
}

func TestWorkerTimeoutScalesWithProbeCount(t *testing.T) {
	db := newTestDB(t)
	store := storage.NewMemoryStorage()
	catalog := []Probe{withDefaults(videoProbe()), withDefaults(galleryProbe())}
	runner := newTestRunner(t, db, store, Config{ProbeTimeout: 10 * time.Minute}, catalog, nil)

	got := NewWorker(runner).Timeout(&river.Job[JobArgs]{})
	if want := 2*10*time.Minute + 5*time.Minute; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
}

func TestJobKindIsStable(t *testing.T) {
	if got := (JobArgs{}).Kind(); got != "canary_sweep" {
		t.Errorf("job kind = %q; renaming it would orphan queued jobs", got)
	}
}
