package canary

import (
	"sort"
	"time"

	"gorm.io/gorm"

	"arker/internal/models"
)

// Health states reported for a platform/post-type slot and for the fleet.
const (
	HealthPassing = "passing"
	HealthFailing = "failing"
	HealthUnknown = "unknown"
)

// SlotHealth is the newest canary result for one platform/post-type slot.
type SlotHealth struct {
	ProbeKey      string     `json:"probe_key"`
	Platform      string     `json:"platform"`
	PostType      string     `json:"post_type"`
	URL           string     `json:"url"`
	Status        string     `json:"status"`
	LastRunAt     *time.Time `json:"last_run_at"`
	StageReached  string     `json:"stage_reached,omitempty"`
	FailureStage  string     `json:"failure_stage,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	ShortID       string     `json:"short_id,omitempty"`
	MediaBytes    int64      `json:"media_bytes,omitempty"`
	MediaCount    int        `json:"media_count,omitempty"`
	ContentType   string     `json:"content_type,omitempty"`
	Provenance    string     `json:"provenance,omitempty"`
	CostUSD       float64    `json:"cost_usd"`
	Trigger       string     `json:"trigger,omitempty"`
}

// Summary is the fleet-level canary signal, embedded additively in /health.
type Summary struct {
	Status       string     `json:"status"`
	Passing      int        `json:"passing"`
	Failing      int        `json:"failing"`
	FailingKeys  []string   `json:"failing_probes,omitempty"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	ScheduleOn   bool       `json:"schedule_enabled"`
	ScheduleSpec string     `json:"schedule,omitempty"`
	Note         string     `json:"note,omitempty"`
}

// LatestPerSlot returns the newest canary run for each platform/post-type.
//
// The subquery form (max id per group) is used rather than a window function so
// the same statement runs on Postgres in production and SQLite in tests.
func LatestPerSlot(db *gorm.DB) ([]models.CanaryRun, error) {
	var runs []models.CanaryRun
	if db == nil {
		return runs, nil
	}
	sub := db.Model(&models.CanaryRun{}).Select("MAX(id)").Group("platform, post_type")
	if err := db.Model(&models.CanaryRun{}).Where("id IN (?)", sub).Order("platform, post_type").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// RecentRuns returns the newest runs across all slots, newest first.
func RecentRuns(db *gorm.DB, limit int) ([]models.CanaryRun, error) {
	var runs []models.CanaryRun
	if db == nil {
		return runs, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	err := db.Model(&models.CanaryRun{}).Order("id DESC").Limit(limit).Find(&runs).Error
	return runs, err
}

// SlotHealthFromRun projects a stored run into the health view.
func SlotHealthFromRun(run models.CanaryRun) SlotHealth {
	status := HealthFailing
	if run.Passed {
		status = HealthPassing
	}
	startedAt := run.StartedAt
	health := SlotHealth{
		ProbeKey: run.ProbeKey, Platform: run.Platform, PostType: run.PostType,
		URL: run.URL, Status: status, LastRunAt: &startedAt,
		StageReached: run.StageReached, FailureStage: run.FailureStage, FailureReason: run.FailureReason,
		ShortID: run.ShortID, MediaBytes: run.MediaBytes, MediaCount: run.MediaCount,
		ContentType: run.ContentType, Provenance: run.Provenance, CostUSD: run.CostUSD,
		Trigger: run.Trigger,
	}
	if health.ProbeKey == "" {
		health.ProbeKey = run.Platform + "/" + run.PostType
	}
	return health
}

// CurrentHealth returns the latest result per slot, sorted by probe key.
func CurrentHealth(db *gorm.DB) ([]SlotHealth, error) {
	runs, err := LatestPerSlot(db)
	if err != nil {
		return nil, err
	}
	health := make([]SlotHealth, 0, len(runs))
	for _, run := range runs {
		health = append(health, SlotHealthFromRun(run))
	}
	sort.Slice(health, func(i, j int) bool { return health[i].ProbeKey < health[j].ProbeKey })
	return health, nil
}

// HealthSummary is the /health view: the fleet signal, computed defensively.
//
// A database or schema problem here must never fail the health endpoint —
// /health is what the orchestrator restarts the container on. An unreadable
// canary history degrades to "unknown" with a note, which is honest and
// harmless, instead of taking production down over a monitoring table.
func HealthSummary(db *gorm.DB, cfg Config) Summary {
	health, err := CurrentHealth(db)
	if err != nil {
		return Summary{
			Status:       HealthUnknown,
			ScheduleOn:   cfg.ScheduleEnabled(),
			ScheduleSpec: cfg.Schedule,
			Note:         "canary history unavailable: " + err.Error(),
		}
	}
	return SummarizeHealth(health, cfg)
}

// SummarizeHealth reduces the per-slot view to the fleet signal used by
// /health.
//
// "unknown" is a distinct state from "passing" on purpose: a fleet that has
// never run a canary has no evidence of health, and reporting it as green
// would be the same unknown-completeness-reads-green failure the archive
// contract forbids.
func SummarizeHealth(health []SlotHealth, cfg Config) Summary {
	summary := Summary{Status: HealthUnknown, ScheduleOn: cfg.ScheduleEnabled(), ScheduleSpec: cfg.Schedule}
	// When the operator has narrowed the fleet with CANARY_PROBES, history rows
	// from slots outside that set no longer describe the configured fleet. A
	// deliberately dropped slot (reddit behind an IP-level WAF) must not keep
	// /health red on its stale last run: that is exactly the alarm-nobody-reads
	// failure the runbook's "do not activate with a known-red slot" rule exists
	// to prevent. An empty ProbeKeys means the default fleet; keep everything.
	if len(cfg.ProbeKeys) > 0 {
		enabled := make(map[string]bool, len(cfg.ProbeKeys))
		for _, key := range cfg.ProbeKeys {
			enabled[key] = true
		}
		kept := make([]SlotHealth, 0, len(health))
		for _, slot := range health {
			if enabled[slot.ProbeKey] {
				kept = append(kept, slot)
			}
		}
		health = kept
	}
	for _, slot := range health {
		if slot.LastRunAt != nil && (summary.LastRunAt == nil || slot.LastRunAt.After(*summary.LastRunAt)) {
			last := *slot.LastRunAt
			summary.LastRunAt = &last
		}
		switch slot.Status {
		case HealthPassing:
			summary.Passing++
		case HealthFailing:
			summary.Failing++
			summary.FailingKeys = append(summary.FailingKeys, slot.ProbeKey)
		}
	}
	switch {
	case summary.Failing > 0:
		summary.Status = HealthFailing
	case summary.Passing > 0:
		summary.Status = HealthPassing
	default:
		summary.Status = HealthUnknown
		if !cfg.ScheduleEnabled() {
			summary.Note = "no canary results yet; recurring canaries are disabled (" + ScheduleEnvVar + " is unset)"
		} else {
			summary.Note = "no canary results yet"
		}
	}
	sort.Strings(summary.FailingKeys)
	return summary
}
