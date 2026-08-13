package canary

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// PaidFallbackDeclarer is implemented by archivers that can reach a paid
// backend. The Bright Data fallback wrapper declares itself; native archivers
// do not implement the interface at all.
//
// This exists so the canary runner can refuse to start rather than trust that
// whoever wired it up passed the native map. The structural guard (canaries
// hold their own archiver map, built before the paid wrappers are applied) is
// the real protection; this is the assertion that the structural guard is
// actually in place, and it fails closed.
type PaidFallbackDeclarer interface {
	PaidFallbackEnabled() bool
}

// AssertNativeOnly returns an error if any archiver in the map can spend money.
func AssertNativeOnly(archiversMap map[string]archivers.Archiver) error {
	for name, arch := range archiversMap {
		declarer, ok := arch.(PaidFallbackDeclarer)
		if ok && declarer.PaidFallbackEnabled() {
			return fmt.Errorf("archiver %q has a paid fallback wired in; canaries run native-only and refuse to start with a billable archiver in scope", name)
		}
	}
	return nil
}

// BudgetDecision is the answer to "may this sweep use the paid fallback?".
type BudgetDecision struct {
	AllowPaid     bool
	Reason        string
	SpentTodayUSD float64
	PerRunCapUSD  float64
	PerDayCapUSD  float64
}

// EvaluateBudget decides whether paid probes are permitted, given today's
// canary spend. It is pure so every branch is reachable in a unit test.
//
// Every "no" is a normal outcome, not an error: the sweep still runs, it just
// runs native-only. The only thing a budget refusal changes is whether a
// native failure is allowed to escalate into a paid retry.
func EvaluateBudget(cfg Config, spentTodayUSD float64) BudgetDecision {
	decision := BudgetDecision{
		SpentTodayUSD: spentTodayUSD,
		PerRunCapUSD:  cfg.MaxCostPerRunUSD,
		PerDayCapUSD:  cfg.MaxCostPerDayUSD,
	}
	switch {
	case !cfg.AllowPaidFallback:
		decision.Reason = "paid canary probes are disabled (CANARY_ALLOW_PAID_FALLBACK is false)"
	case cfg.MaxCostPerRunUSD <= 0:
		decision.Reason = "per-run budget ceiling is $0.00 (CANARY_MAX_COST_USD_PER_RUN)"
	case cfg.MaxCostPerDayUSD <= 0:
		decision.Reason = "daily budget ceiling is $0.00 (CANARY_MAX_COST_USD_PER_DAY)"
	case spentTodayUSD+cfg.MaxCostPerRunUSD > cfg.MaxCostPerDayUSD:
		decision.Reason = fmt.Sprintf("daily canary budget would be exceeded: $%.4f spent today, per-run ceiling $%.4f, daily ceiling $%.4f",
			spentTodayUSD, cfg.MaxCostPerRunUSD, cfg.MaxCostPerDayUSD)
	default:
		decision.AllowPaid = true
		decision.Reason = fmt.Sprintf("paid probes permitted: $%.4f spent today of a $%.4f daily ceiling", spentTodayUSD, cfg.MaxCostPerDayUSD)
	}
	return decision
}

// SpendForShortID sums the Bright Data operations and estimated cost recorded
// against one capture. Canary validation calls this after every probe: a
// non-zero result on a native-only sweep means money was spent somewhere the
// guard did not cover, and the probe fails loudly instead of passing quietly.
func SpendForShortID(db *gorm.DB, shortID string) (Spend, error) {
	var spend Spend
	if db == nil || shortID == "" {
		return spend, nil
	}
	var row struct {
		Operations int64
		CostUSD    float64
	}
	err := db.Model(&models.BrightDataUsage{}).
		Select("COUNT(*) AS operations, COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Where("short_id = ?", shortID).
		Scan(&row).Error
	if err != nil {
		return spend, err
	}
	return Spend{Operations: row.Operations, CostUSD: row.CostUSD}, nil
}

// CanarySpendSince sums Bright Data cost attributed to canary captures started
// at or after `since`. Attribution is by short ID: canary_runs records the
// capture each probe created, and bright_data_usages records spend per capture,
// so canary spend is exactly the intersection — no separate accounting to drift.
func CanarySpendSince(db *gorm.DB, since time.Time) (float64, error) {
	if db == nil {
		return 0, nil
	}
	var total float64
	err := db.Model(&models.BrightDataUsage{}).
		Select("COALESCE(SUM(cost_usd), 0)").
		Where("short_id IN (?)",
			db.Model(&models.CanaryRun{}).Select("short_id").Where("short_id != '' AND started_at >= ?", since)).
		Scan(&total).Error
	return total, err
}

// StartOfUTCDay is the daily budget window boundary. UTC rather than local
// time so the ceiling does not shift under a container timezone change.
func StartOfUTCDay(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
