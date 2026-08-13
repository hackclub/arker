package canary

import (
	"testing"
	"time"
)

// The default configuration must be inert. If this test ever fails, canaries
// have become opt-out rather than opt-in.
func TestDefaultConfigIsDisabled(t *testing.T) {
	cfg, err := LoadConfig(RawConfig{}, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ScheduleEnabled() {
		t.Error("canaries are scheduled with no configuration set")
	}
	if cfg.AllowPaidFallback {
		t.Error("paid canary probes are enabled by default")
	}
	if job := PeriodicJob(cfg); job != nil {
		t.Error("a periodic job was registered with no schedule configured")
	}
}

func TestParseSchedule(t *testing.T) {
	cases := []struct {
		value    string
		want     time.Duration
		wantErr  bool
		disabled bool
	}{
		{value: "", disabled: true},
		{value: "  ", disabled: true},
		{value: "off", disabled: true},
		{value: "OFF", disabled: true},
		{value: "disabled", disabled: true},
		{value: "none", disabled: true},
		{value: "0", disabled: true},
		{value: "6h", want: 6 * time.Hour},
		{value: "15m", want: 15 * time.Minute},
		{value: "90m", want: 90 * time.Minute},
		{value: "5m", wantErr: true},   // below the floor
		{value: "1s", wantErr: true},   // below the floor
		{value: "soon", wantErr: true}, // not a duration
		{value: "6", wantErr: true},    // no unit
	}
	for _, tc := range cases {
		got, err := ParseSchedule(tc.value)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("ParseSchedule(%q) = %v, want an error", tc.value, got)
		case !tc.wantErr && err != nil:
			t.Errorf("ParseSchedule(%q) errored: %v", tc.value, err)
		case tc.disabled && got != 0:
			t.Errorf("ParseSchedule(%q) = %v, want disabled", tc.value, got)
		case !tc.disabled && !tc.wantErr && got != tc.want:
			t.Errorf("ParseSchedule(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A bad schedule must not produce a usable config, and must not be fatal
// either: the caller logs it and carries on with canaries off.
func TestLoadConfigRejectsBadScheduleWithoutEnabling(t *testing.T) {
	cfg, err := LoadConfig(RawConfig{Schedule: "5m"}, nil)
	if err == nil {
		t.Fatal("expected an error for a sub-minimum schedule")
	}
	if cfg.ScheduleEnabled() {
		t.Error("a rejected schedule still enabled canaries")
	}
	if job := PeriodicJob(cfg); job != nil {
		t.Error("a rejected schedule still registered a periodic job")
	}
}

func TestLoadConfigParsesProbeSelectionAndOverrides(t *testing.T) {
	raw := RawConfig{
		Schedule:         "6h",
		Probes:           "youtube/video, VIMEO/VIDEO ,,",
		MaxCostPerRunUSD: 0.25,
		MaxCostPerDayUSD: 1,
		ProbeTimeout:     20 * time.Minute,
	}
	environ := []string{
		"PATH=/usr/bin",
		"CANARY_PROBE_URL_YOUTUBE_VIDEO=https://www.youtube.com/watch?v=OVERRIDE",
		"CANARY_PROBE_URL_EMPTY=",
		"NOT_A_CANARY_VAR=x",
	}
	cfg, err := LoadConfig(raw, environ)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.ScheduleEnabled() || cfg.Interval != 6*time.Hour {
		t.Fatalf("interval = %v, want 6h enabled", cfg.Interval)
	}
	if len(cfg.ProbeKeys) != 2 || cfg.ProbeKeys[0] != "youtube/video" || cfg.ProbeKeys[1] != "vimeo/video" {
		t.Errorf("probe keys = %v, want [youtube/video vimeo/video]", cfg.ProbeKeys)
	}
	if got := cfg.ProbeURLOverrides["YOUTUBE_VIDEO"]; got != "https://www.youtube.com/watch?v=OVERRIDE" {
		t.Errorf("override = %q, want the configured URL", got)
	}
	if _, ok := cfg.ProbeURLOverrides["EMPTY"]; ok {
		t.Error("an empty override value should be ignored")
	}
	if cfg.EffectiveProbeTimeout() != 20*time.Minute {
		t.Errorf("probe timeout = %v, want 20m", cfg.EffectiveProbeTimeout())
	}
}

func TestEffectiveProbeTimeoutFallsBack(t *testing.T) {
	if got := (Config{}).EffectiveProbeTimeout(); got != DefaultProbeTimeout {
		t.Errorf("probe timeout = %v, want the %v default", got, DefaultProbeTimeout)
	}
}

// A typo'd count must not silently disable a probe's completeness check.
func TestMediaCountOverridesIgnoreJunkValues(t *testing.T) {
	cfg, err := LoadConfig(RawConfig{}, []string{
		"CANARY_PROBE_MEDIA_COUNT_REDDIT_GALLERY=6",
		"CANARY_PROBE_MEDIA_COUNT_IMGUR_ALBUM=lots",
		"CANARY_PROBE_MEDIA_COUNT_BLUESKY_POST=0",
		"CANARY_PROBE_MEDIA_COUNT_VIMEO_VIDEO=-3",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ProbeMediaCountOverrides["REDDIT_GALLERY"]; got != 6 {
		t.Errorf("reddit override = %d, want 6", got)
	}
	for _, key := range []string{"IMGUR_ALBUM", "BLUESKY_POST", "VIMEO_VIDEO"} {
		if _, ok := cfg.ProbeMediaCountOverrides[key]; ok {
			t.Errorf("%s override was accepted despite an invalid value", key)
		}
	}
}
