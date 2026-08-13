package canary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
	"arker/internal/workers"
)

// Triggers recorded on a canary run.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// maxFailureReasonBytes keeps one pathological extractor error from bloating
// every row in the history table.
const maxFailureReasonBytes = 2000

// ArchiveFunc runs one archive item to completion with the given archiver map.
// Production passes workers.RunArchiveItemInline; tests pass a stub, which is
// what keeps the runner's own logic reachable offline with no extractor,
// network, or browser.
type ArchiveFunc func(ctx context.Context, args workers.ArchiveJobArgs, item *models.ArchiveItem, archiversMap map[string]archivers.Archiver) error

// RunResult is one probe's outcome within a sweep.
type RunResult struct {
	ProbeKey    string     `json:"probe_key"`
	Platform    string     `json:"platform"`
	PostType    string     `json:"post_type"`
	URL         string     `json:"url"`
	ShortID     string     `json:"short_id,omitempty"`
	RunID       uint       `json:"run_id,omitempty"`
	Passed      bool       `json:"passed"`
	Validation  Validation `json:"-"`
	Stage       string     `json:"stage_reached"`
	Failure     string     `json:"failure_stage,omitempty"`
	Reason      string     `json:"failure_reason,omitempty"`
	MediaBytes  int64      `json:"media_bytes,omitempty"`
	MediaCount  int        `json:"media_count,omitempty"`
	ContentType string     `json:"content_type,omitempty"`
	Provenance  string     `json:"provenance,omitempty"`
	CostUSD     float64    `json:"cost_usd"`
	DurationMS  int64      `json:"duration_ms"`
}

// SweepResult is the outcome of a full canary pass.
type SweepResult struct {
	Trigger      string      `json:"trigger"`
	StartedAt    time.Time   `json:"started_at"`
	FinishedAt   time.Time   `json:"finished_at"`
	Results      []RunResult `json:"results"`
	Passed       int         `json:"passed"`
	Failed       int         `json:"failed"`
	Skipped      []string    `json:"skipped,omitempty"`
	PaidAllowed  bool        `json:"paid_allowed"`
	BudgetReason string      `json:"budget_reason"`
	Aborted      string      `json:"aborted,omitempty"`
}

// SweepOptions selects what a sweep runs.
type SweepOptions struct {
	// Trigger is "schedule" or "manual".
	Trigger string
	// Platform narrows the sweep to one platform ("youtube") or one probe key
	// ("youtube/short"). Empty runs every selected probe.
	Platform string
}

// ErrSweepInProgress is returned when a sweep is asked for while one is
// already running. Canary sweeps are serialized: two concurrent sweeps would
// double the unattended traffic each platform sees and make the history
// ambiguous about which run observed what.
var ErrSweepInProgress = errors.New("a canary sweep is already running")

// Runner executes canary probes.
//
// The archiver map it holds is its cost guard. Production builds it from the
// native archivers *before* the Bright Data wrappers are applied, so a probe
// physically cannot reach a billable backend: there is no paid archiver in
// scope to call. AssertNativeOnly re-checks that at the start of every sweep
// and refuses to run if the wiring ever changes.
type Runner struct {
	db    *gorm.DB
	store storage.Storage
	// nativeArchivers must contain no billable archiver. It is what every
	// probe runs on unless paid probes are both opted into and within budget.
	nativeArchivers map[string]archivers.Archiver
	// paidArchivers is the fallback-wrapped map, used only when paid canaries
	// are enabled and today's budget still allows them. Nil in the default
	// configuration.
	paidArchivers map[string]archivers.Archiver
	cfg           Config
	catalog       []Probe

	now               func() time.Time
	archiveFn         ArchiveFunc
	cookiesConfigured func() bool

	mu      sync.Mutex
	running bool
}

// Options configure a Runner. Everything but the first four fields has a
// production default and exists for tests.
type Options struct {
	DB *gorm.DB
	// Store must be the same storage instance the archivers write to.
	Store storage.Storage
	// NativeArchivers must be native-only; see Runner. Build it from the
	// archiver map *before* any paid wrapper is applied.
	NativeArchivers map[string]archivers.Archiver
	// PaidArchivers is optional and only used when paid canaries are enabled
	// and within budget. Leave it nil unless that is what you want.
	PaidArchivers map[string]archivers.Archiver
	Config        Config
	// Catalog defaults to DefaultProbes().
	Catalog           []Probe
	Now               func() time.Time
	ArchiveFn         ArchiveFunc
	CookiesConfigured func() bool
}

// New builds a Runner.
func New(opts Options) *Runner {
	r := &Runner{
		db:                opts.DB,
		store:             opts.Store,
		nativeArchivers:   opts.NativeArchivers,
		paidArchivers:     opts.PaidArchivers,
		cfg:               opts.Config,
		catalog:           opts.Catalog,
		now:               opts.Now,
		archiveFn:         opts.ArchiveFn,
		cookiesConfigured: opts.CookiesConfigured,
	}
	if r.catalog == nil {
		r.catalog = DefaultProbes()
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.cookiesConfigured == nil {
		r.cookiesConfigured = utils.MediaCookiesConfigured
	}
	if r.archiveFn == nil {
		r.archiveFn = func(ctx context.Context, args workers.ArchiveJobArgs, item *models.ArchiveItem, archiversMap map[string]archivers.Archiver) error {
			return workers.RunArchiveItemInline(ctx, args, item, r.store, r.db, archiversMap)
		}
	}
	return r
}

// archiversFor picks the archiver map for a sweep. Paid archivers are only
// reachable when paid canaries are configured AND this sweep's budget check
// said yes; every other path gets the native-only map.
func (r *Runner) archiversFor(paidAllowed bool) map[string]archivers.Archiver {
	if paidAllowed && r.paidArchivers != nil {
		return r.paidArchivers
	}
	return r.nativeArchivers
}

// Config returns the runner's configuration.
func (r *Runner) Config() Config { return r.cfg }

// Probes returns the probes this runner would sweep, plus any configuration
// problems that removed one (an unknown key, or a cookies-required probe with
// no cookie jar).
func (r *Runner) Probes() ([]Probe, []error) {
	return SelectProbes(r.cfg, r.catalog, r.cookiesConfigured())
}

// RunSweep archives every selected probe and validates the whole contract on
// each result.
//
// Probes run sequentially. A canary sweep is unattended traffic against other
// people's platforms; running six downloads in parallel every six hours is how
// a monitoring system turns into a rate-limit incident.
func (r *Runner) RunSweep(ctx context.Context, opts SweepOptions) (SweepResult, error) {
	if !r.begin() {
		return SweepResult{}, ErrSweepInProgress
	}
	defer r.finish()

	trigger := opts.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	started := r.now()
	result := SweepResult{Trigger: trigger, StartedAt: started}

	spentToday, err := CanarySpendSince(r.db, StartOfUTCDay(started))
	if err != nil {
		slog.Warn("Could not read today's canary spend; treating paid probes as unaffordable", "error", err)
		spentToday = r.cfg.MaxCostPerDayUSD
	}
	budget := EvaluateBudget(r.cfg, spentToday)
	result.PaidAllowed, result.BudgetReason = budget.AllowPaid, budget.Reason

	// The cost guard. The native map is the one probes run on unless paid
	// canaries are both opted into and inside today's budget, so a billable
	// archiver in it is a wiring bug — and the sweep refuses to run rather than
	// find out what it costs.
	if err := AssertNativeOnly(r.nativeArchivers); err != nil {
		result.Aborted = err.Error()
		result.FinishedAt = r.now()
		slog.Error("Canary sweep aborted before spending money", "error", err, "trigger", trigger)
		return result, err
	}

	probes, problems := r.Probes()
	for _, problem := range problems {
		result.Skipped = append(result.Skipped, problem.Error())
		slog.Warn("Canary probe skipped", "reason", problem.Error())
	}
	probes = FilterByPlatform(probes, opts.Platform)
	if len(probes) == 0 {
		result.FinishedAt = r.now()
		return result, nil
	}

	for _, probe := range probes {
		if ctx.Err() != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: sweep cancelled before it ran", probe.Key()))
			continue
		}
		runResult := r.runProbe(ctx, probe, trigger, budget.AllowPaid)
		if runResult.Passed {
			result.Passed++
		} else {
			result.Failed++
		}
		result.Results = append(result.Results, runResult)
	}
	result.FinishedAt = r.now()

	slog.Info("Canary sweep finished",
		"trigger", trigger, "passed", result.Passed, "failed", result.Failed,
		"paid_allowed", result.PaidAllowed, "duration", result.FinishedAt.Sub(result.StartedAt).Round(time.Second))
	return result, nil
}

// InProgress reports whether a sweep is running right now. Advisory: the
// runner's own lock is what actually serializes sweeps.
func (r *Runner) InProgress() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *Runner) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *Runner) finish() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// runProbe archives one probe and validates it, recording a canary_runs row
// either way.
//
// The row is written before the archive starts. A probe that panics, is killed
// mid-download, or hangs until the process restarts leaves a row with no
// FinishedAt, which is visibly different from both a pass and a clean failure —
// silence is the one outcome a monitoring system may not produce.
func (r *Runner) runProbe(ctx context.Context, probe Probe, trigger string, paidAllowed bool) RunResult {
	started := r.now()
	run := models.CanaryRun{
		ProbeKey: probe.Key(), Platform: probe.Platform, PostType: probe.PostType,
		URL: probe.URL, Trigger: trigger, ArchiveType: probe.ExpectedType,
		StartedAt: started, PaidAllowed: paidAllowed,
	}
	if r.db != nil {
		if err := r.db.Create(&run).Error; err != nil {
			slog.Error("Failed to record canary run start", "probe", probe.Key(), "error", err)
		}
	}

	validation, shortID := r.executeProbe(ctx, probe, paidAllowed)

	finished := r.now()
	spend := Spend{}
	if shortID != "" {
		if s, err := SpendForShortID(r.db, shortID); err == nil {
			spend = s
		} else {
			slog.Warn("Could not read canary spend", "probe", probe.Key(), "short_id", shortID, "error", err)
		}
	}
	if validation.Passed && spend.CostUSD > 0 && !paidAllowed {
		// Belt and braces: ValidateArchive already fails on recorded spend, but
		// a probe that never reached validation could still have billed.
		validation = fail(validation.StageReached, StageProvenance,
			"%d paid Bright Data operation(s) costing $%.4f were recorded against this capture; canaries must never spend money",
			spend.Operations, spend.CostUSD)
	}

	result := RunResult{
		ProbeKey: probe.Key(), Platform: probe.Platform, PostType: probe.PostType,
		URL: probe.URL, ShortID: shortID, RunID: run.ID,
		Passed: validation.Passed, Validation: validation,
		Stage: validation.StageReached, Failure: validation.FailureStage, Reason: validation.FailureReason,
		MediaBytes: validation.MediaBytes, MediaCount: validation.MediaCount,
		ContentType: validation.ContentType, Provenance: validation.Provenance,
		CostUSD: spend.CostUSD, DurationMS: finished.Sub(started).Milliseconds(),
	}

	if r.db != nil && run.ID != 0 {
		updates := map[string]any{
			"finished_at":    finished,
			"duration_ms":    result.DurationMS,
			"stage_reached":  validation.StageReached,
			"passed":         validation.Passed,
			"failure_stage":  validation.FailureStage,
			"failure_reason": truncate(validation.FailureReason, maxFailureReasonBytes),
			"short_id":       shortID,
			"media_bytes":    validation.MediaBytes,
			"media_count":    validation.MediaCount,
			"content_type":   validation.ContentType,
			"provenance":     validation.Provenance,
			"cost_usd":       spend.CostUSD,
		}
		if err := r.db.Model(&models.CanaryRun{}).Where("id = ?", run.ID).Updates(updates).Error; err != nil {
			slog.Error("Failed to record canary run result", "probe", probe.Key(), "error", err)
		}
	}

	// This is the alerting surface. A failing canary is a production archive
	// contract violation on a URL known to have worked, so it logs at ERROR
	// with everything needed to act: which slot, how far it got, why it
	// stopped, and the capture to open.
	if validation.Passed {
		slog.Info("Canary probe passed",
			"probe", probe.Key(), "url", probe.URL, "short_id", shortID,
			"media_bytes", validation.MediaBytes, "media_count", validation.MediaCount,
			"provenance", validation.Provenance, "duration_ms", result.DurationMS)
		for _, warning := range validation.Warnings {
			slog.Warn("Canary probe warning", "probe", probe.Key(), "short_id", shortID, "warning", warning)
		}
	} else {
		slog.Error("CANARY FAILED",
			"probe", probe.Key(), "platform", probe.Platform, "post_type", probe.PostType,
			"url", probe.URL, "short_id", shortID,
			"stage_reached", validation.StageReached, "failure_stage", validation.FailureStage,
			"failure_reason", validation.FailureReason,
			"provenance", validation.Provenance, "cost_usd", spend.CostUSD,
			"duration_ms", result.DurationMS, "trigger", trigger)
	}
	return result
}

// executeProbe does the work: routing check, capture creation, archive, and
// contract validation. It returns the verdict plus the short ID of whatever
// capture it created (empty if it never got that far).
func (r *Runner) executeProbe(ctx context.Context, probe Probe, paidAllowed bool) (Validation, string) {
	if routing := ValidateRouting(probe); !routing.Passed {
		return routing, ""
	}
	if r.db == nil {
		return fail(StageRouting, StageCapture, "canary runner has no database handle"), ""
	}

	shortID, err := workers.CreateCaptureRows(r.db, probe.URL, []string{probe.ExpectedType}, nil)
	if err != nil {
		return fail(StageRouting, StageCapture, "could not create the probe capture: %v", redact(err.Error())), ""
	}

	var item models.ArchiveItem
	if err := r.db.Joins("JOIN captures ON archive_items.capture_id = captures.id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, probe.ExpectedType).
		First(&item).Error; err != nil {
		return fail(StageCapture, StageCapture, "probe capture %s has no %s item: %v", shortID, probe.ExpectedType, redact(err.Error())), shortID
	}

	probeCtx, cancel := context.WithTimeout(ctx, r.cfg.EffectiveProbeTimeout())
	defer cancel()

	args := workers.ArchiveJobArgs{ShortID: shortID, Type: probe.ExpectedType, URL: probe.URL}
	archiveErr := r.archiveFn(probeCtx, args, &item, r.archiversFor(paidAllowed))

	var fresh models.ArchiveItem
	if err := r.db.First(&fresh, item.ID).Error; err != nil {
		return fail(StageCapture, StageArchive, "probe archive item %d could not be reloaded: %v", item.ID, redact(err.Error())), shortID
	}

	spend, spendErr := SpendForShortID(r.db, shortID)
	if spendErr != nil {
		slog.Warn("Could not read canary spend during validation", "short_id", shortID, "error", spendErr)
	}

	if archiveErr != nil {
		v := fail(StageCapture, StageArchive, "archive failed on the free native path: %v", redact(archiveErr.Error()))
		v.Provenance = provenanceOf(fresh)
		return v, shortID
	}
	return ValidateArchive(probe, &fresh, r.store, spend, r.cfg.AllowPaidFallback), shortID
}

func provenanceOf(item models.ArchiveItem) string {
	if item.Source == "" {
		return models.ArchiveSourceNative
	}
	return item.Source
}

// redact strips configured secrets (currently the media proxy URL, which can
// embed credentials) out of anything headed for the database or the logs.
func redact(s string) string {
	return utils.RedactSecrets(s, utils.YtDlpProxyRedactionSecrets())
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… (truncated)"
}
