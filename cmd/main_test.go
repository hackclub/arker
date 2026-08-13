package main

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kelseyhightower/envconfig"

	"arker/internal/canary"
)

func TestNormalizeGinModeDefaultsToRelease(t *testing.T) {
	tests := map[string]string{
		"":        gin.ReleaseMode,
		"release": gin.ReleaseMode,
		"debug":   gin.DebugMode,
		"test":    gin.TestMode,
		"bad":     gin.ReleaseMode,
	}

	for input, want := range tests {
		if got := normalizeGinMode(input); got != want {
			t.Fatalf("normalizeGinMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got := parseTrustedProxies("127.0.0.1, ::1, 172.16.0.0/12,")
	want := []string{"127.0.0.1", "::1", "172.16.0.0/12"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTrustedProxies returned %#v, want %#v", got, want)
	}
}

// The activation lever in docs/canaries.md is literally CANARY_SCHEDULE. It is
// an embedded struct so envconfig keeps that exact name; a named field would
// silently become CANARY_CANARY_SCHEDULE and the documented activation step
// would do nothing at all.
func TestCanaryEnvVarNames(t *testing.T) {
	t.Setenv("CANARY_SCHEDULE", "6h")
	t.Setenv("CANARY_PROBES", "youtube/video")
	t.Setenv("CANARY_ALLOW_PAID_FALLBACK", "true")
	t.Setenv("CANARY_MAX_COST_USD_PER_DAY", "2.50")
	t.Setenv("CANARY_PROBE_TIMEOUT", "20m")

	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		t.Fatalf("envconfig: %v", err)
	}
	if cfg.Schedule != "6h" {
		t.Errorf("CANARY_SCHEDULE did not reach Config.Schedule (got %q)", cfg.Schedule)
	}
	if cfg.Probes != "youtube/video" {
		t.Errorf("CANARY_PROBES did not reach Config.Probes (got %q)", cfg.Probes)
	}
	if !cfg.AllowPaidFallback {
		t.Error("CANARY_ALLOW_PAID_FALLBACK did not reach Config.AllowPaidFallback")
	}
	if cfg.MaxCostPerDayUSD != 2.50 {
		t.Errorf("CANARY_MAX_COST_USD_PER_DAY = %v, want 2.50", cfg.MaxCostPerDayUSD)
	}
	if cfg.ProbeTimeout != 20*time.Minute {
		t.Errorf("CANARY_PROBE_TIMEOUT = %v, want 20m", cfg.ProbeTimeout)
	}

	parsed, err := canary.LoadConfig(cfg.RawConfig, os.Environ())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !parsed.ScheduleEnabled() || parsed.Interval != 6*time.Hour {
		t.Fatalf("parsed schedule = %v (enabled=%v), want 6h enabled", parsed.Interval, parsed.ScheduleEnabled())
	}
	if canary.PeriodicJob(parsed) == nil {
		t.Error("no periodic job registered for the documented activation value")
	}
}

// The inverse, and the more important half: with nothing set, nothing runs.
func TestCanaryDefaultsAreInert(t *testing.T) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		t.Fatalf("envconfig: %v", err)
	}
	parsed, err := canary.LoadConfig(cfg.RawConfig, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if parsed.ScheduleEnabled() {
		t.Error("canaries are scheduled with no environment configuration")
	}
	if parsed.AllowPaidFallback {
		t.Error("paid canary probes are enabled by default")
	}
	if canary.PeriodicJob(parsed) != nil {
		t.Error("a periodic job exists with no CANARY_SCHEDULE set")
	}
}
