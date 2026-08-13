package canary

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// nativeStub stands in for an ordinary archiver: it does not implement
// PaidFallbackDeclarer at all.
type nativeStub struct{}

func (nativeStub) Archive(context.Context, string, io.Writer, *gorm.DB, uint) (archivers.Result, error) {
	return archivers.Result{}, nil
}

// paidStub stands in for the Bright Data fallback wrapper.
type paidStub struct{ nativeStub }

func (paidStub) PaidFallbackEnabled() bool { return true }

func TestAssertNativeOnly(t *testing.T) {
	native := map[string]archivers.Archiver{"yt-dlp": nativeStub{}, "gallery-dl": nativeStub{}}
	if err := AssertNativeOnly(native); err != nil {
		t.Fatalf("native-only map rejected: %v", err)
	}

	withPaid := map[string]archivers.Archiver{"yt-dlp": paidStub{}, "gallery-dl": nativeStub{}}
	err := AssertNativeOnly(withPaid)
	if err == nil {
		t.Fatal("a billable archiver passed the native-only assertion")
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error %q should name the offending archiver", err)
	}
}

func TestEvaluateBudget(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		spentToday float64
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "paid probes disabled by default",
			cfg:        Config{MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
			wantReason: "CANARY_ALLOW_PAID_FALLBACK",
		},
		{
			name:       "zero per-run ceiling",
			cfg:        Config{AllowPaidFallback: true, MaxCostPerDayUSD: 1},
			wantReason: "per-run budget ceiling is $0.00",
		},
		{
			name:       "zero daily ceiling",
			cfg:        Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25},
			wantReason: "daily budget ceiling is $0.00",
		},
		{
			name:       "daily ceiling already consumed",
			cfg:        Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
			spentToday: 0.90,
			wantReason: "daily canary budget would be exceeded",
		},
		{
			name:      "within budget",
			cfg:       Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
			wantAllow: true,
		},
		{
			name:       "exactly at the ceiling is allowed",
			cfg:        Config{AllowPaidFallback: true, MaxCostPerRunUSD: 0.25, MaxCostPerDayUSD: 1},
			spentToday: 0.75,
			wantAllow:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateBudget(tc.cfg, tc.spentToday)
			if got.AllowPaid != tc.wantAllow {
				t.Errorf("AllowPaid = %v, want %v (reason: %s)", got.AllowPaid, tc.wantAllow, got.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, tc.wantReason)
			}
			if got.SpentTodayUSD != tc.spentToday {
				t.Errorf("SpentTodayUSD = %v, want %v", got.SpentTodayUSD, tc.spentToday)
			}
		})
	}
}

func TestSpendForShortID(t *testing.T) {
	db := newTestDB(t)
	db.Create(&models.BrightDataUsage{ShortID: "abc12", Product: "web_scraper", CostUSD: 0.0015, Success: true})
	db.Create(&models.BrightDataUsage{ShortID: "abc12", Product: "browser_api", CostUSD: 0.0200, Success: false})
	db.Create(&models.BrightDataUsage{ShortID: "other", Product: "web_scraper", CostUSD: 5, Success: true})

	spend, err := SpendForShortID(db, "abc12")
	if err != nil {
		t.Fatalf("SpendForShortID: %v", err)
	}
	if spend.Operations != 2 {
		t.Errorf("operations = %d, want 2 (failed attempts bill too)", spend.Operations)
	}
	if spend.CostUSD < 0.0214 || spend.CostUSD > 0.0216 {
		t.Errorf("cost = %v, want ~0.0215", spend.CostUSD)
	}

	empty, err := SpendForShortID(db, "nosuch")
	if err != nil || empty.Operations != 0 || empty.CostUSD != 0 {
		t.Errorf("unknown short ID gave %+v (err %v), want zero spend", empty, err)
	}
}

// Canary spend is attributed by joining canary_runs to bright_data_usages on
// short ID, so it cannot drift from the invoice-facing table.
func TestCanarySpendSince(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	today := StartOfUTCDay(now)

	db.Create(&models.CanaryRun{ProbeKey: "youtube/video", ShortID: "todayA", StartedAt: now})
	db.Create(&models.CanaryRun{ProbeKey: "vimeo/video", ShortID: "yesterday", StartedAt: today.Add(-3 * time.Hour)})
	db.Create(&models.BrightDataUsage{ShortID: "todayA", CostUSD: 0.10})
	db.Create(&models.BrightDataUsage{ShortID: "yesterday", CostUSD: 0.50})
	// A non-canary capture's spend must not count against the canary budget.
	db.Create(&models.BrightDataUsage{ShortID: "userCap", CostUSD: 7.00})

	spent, err := CanarySpendSince(db, today)
	if err != nil {
		t.Fatalf("CanarySpendSince: %v", err)
	}
	if spent < 0.099 || spent > 0.101 {
		t.Errorf("today's canary spend = %v, want 0.10", spent)
	}
}

func TestStartOfUTCDay(t *testing.T) {
	got := StartOfUTCDay(time.Date(2026, 8, 12, 23, 45, 0, 0, time.FixedZone("EDT", -4*3600)))
	want := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("StartOfUTCDay = %v, want %v (the window is UTC, not local)", got, want)
	}
}
