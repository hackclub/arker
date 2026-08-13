package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/canary"
)

// CanaryService is the handler's view of the canary runner, narrow enough that
// tests can substitute a fake and never touch an archiver.
type CanaryService interface {
	Config() canary.Config
	Probes() ([]canary.Probe, []error)
	InProgress() bool
	RunSweep(ctx context.Context, opts canary.SweepOptions) (canary.SweepResult, error)
}

// manualSweepTimeout bounds a detached manual sweep. Long enough for every
// probe to download real media, short enough that a wedged sweep releases the
// runner before the next scheduled one.
const manualSweepTimeout = 2 * time.Hour

type canaryProbeView struct {
	Key              string `json:"key"`
	Platform         string `json:"platform"`
	PostType         string `json:"post_type"`
	URL              string `json:"url"`
	ArchiveType      string `json:"archive_type"`
	ExpectedMedia    string `json:"expected_media"`
	MinMediaBytes    int64  `json:"min_media_bytes"`
	MinMediaCount    int    `json:"min_media_count"`
	RequiresCookies  bool   `json:"requires_cookies"`
	DefaultEnabled   bool   `json:"default_enabled"`
	URLOverrideEnv   string `json:"url_override_env"`
	CountOverrideEnv string `json:"media_count_override_env"`
	Note             string `json:"note,omitempty"`
}

// CanariesGet reports current canary health plus recent history.
//
// It answers three questions an operator has at once: is the recurring check
// even turned on, what is each platform's latest verdict, and what has been
// happening. Configuration is included because a canary system that is
// silently disabled looks exactly like one where everything passes.
func CanariesGet(c *gin.Context, db *gorm.DB, svc CanaryService) {
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "canaries are not configured on this server"})
		return
	}
	cfg := svc.Config()

	health, err := canary.CurrentHealth(db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	summary := canary.SummarizeHealth(health, cfg)

	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if parsed, convErr := strconv.Atoi(raw); convErr == nil && parsed > 0 {
			limit = parsed
		}
	}
	recent, err := canary.RecentRuns(db, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	probes, problems := svc.Probes()
	probeViews := make([]canaryProbeView, 0, len(probes))
	for _, probe := range probes {
		probeViews = append(probeViews, canaryProbeView{
			Key: probe.Key(), Platform: probe.Platform, PostType: probe.PostType,
			URL: probe.URL, ArchiveType: probe.ExpectedType, ExpectedMedia: probe.ExpectedMedia,
			MinMediaBytes: probe.MinMediaBytes, MinMediaCount: probe.MinMediaCount,
			RequiresCookies: probe.RequiresCookies, DefaultEnabled: probe.DefaultEnabled,
			URLOverrideEnv:   canary.ProbeURLEnvPrefix + probe.EnvKey(),
			CountOverrideEnv: canary.ProbeMediaCountEnvPrefix + probe.EnvKey(),
			Note:             probe.Note,
		})
	}
	skipped := make([]string, 0, len(problems))
	for _, problem := range problems {
		skipped = append(skipped, problem.Error())
	}

	spentToday, spendErr := canary.CanarySpendSince(db, canary.StartOfUTCDay(time.Now()))
	if spendErr != nil {
		slog.Warn("Could not read canary spend for admin view", "error", spendErr)
	}

	c.JSON(http.StatusOK, gin.H{
		"schedule": gin.H{
			"enabled":          cfg.ScheduleEnabled(),
			"value":            cfg.Schedule,
			"interval_seconds": int64(cfg.Interval / time.Second),
			"env_var":          canary.ScheduleEnvVar,
			"sweep_running":    svc.InProgress(),
		},
		"paid_fallback": gin.H{
			"allowed":              cfg.AllowPaidFallback,
			"max_cost_per_run_usd": cfg.MaxCostPerRunUSD,
			"max_cost_per_day_usd": cfg.MaxCostPerDayUSD,
			"spent_today_usd":      spentToday,
			"note":                 "Canaries run on the free native path. Paid probes are a separate opt-in and are not part of enabling the schedule.",
		},
		"summary": summary,
		"health":  health,
		"probes":  probeViews,
		"skipped": skipped,
		"recent":  recent,
	})
}

// CanariesRun triggers a sweep by hand: all probes, or ?platform=youtube for
// one platform (a probe key like youtube/short also works).
//
// It detaches by default and returns 202, because a full sweep downloads real
// media and would otherwise sit on the request past any sane proxy timeout.
// ?wait=1 runs it inline and returns the verdicts, which is what the
// pre-activation check in docs/canaries.md uses.
func CanariesRun(c *gin.Context, db *gorm.DB, svc CanaryService) {
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "canaries are not configured on this server"})
		return
	}
	platform := c.Query("platform")
	opts := canary.SweepOptions{Trigger: canary.TriggerManual, Platform: platform}

	if svc.InProgress() {
		c.JSON(http.StatusConflict, gin.H{"error": canary.ErrSweepInProgress.Error()})
		return
	}

	if isTruthy(c.Query("wait")) {
		result, err := svc.RunSweep(c.Request.Context(), opts)
		if err != nil {
			status := http.StatusInternalServerError
			if err == canary.ErrSweepInProgress {
				status = http.StatusConflict
			}
			c.JSON(status, gin.H{"error": err.Error(), "result": result})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}

	go func() {
		// Detached from the request: the operator's connection closing must not
		// abandon a half-finished sweep and leave rows with no verdict.
		ctx, cancel := context.WithTimeout(context.Background(), manualSweepTimeout)
		defer cancel()
		if _, err := svc.RunSweep(ctx, opts); err != nil {
			slog.Error("Manual canary sweep failed to start", "error", err, "platform", platform)
		}
	}()

	probes, _ := svc.Probes()
	keys := make([]string, 0, len(probes))
	for _, probe := range canary.FilterByPlatform(probes, platform) {
		keys = append(keys, probe.Key())
	}
	c.JSON(http.StatusAccepted, gin.H{
		"status":   "started",
		"platform": platform,
		"probes":   keys,
		"note":     "Sweep runs in the background; poll GET /admin/canaries for results, or re-run with ?wait=1 to block.",
	})
}

func isTruthy(value string) bool {
	switch value {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
