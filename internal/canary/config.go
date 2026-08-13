package canary

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// ScheduleEnvVar is the single switch that turns recurring canaries on.
	// Empty (the default) means no periodic job is registered at all.
	ScheduleEnvVar = "CANARY_SCHEDULE"
	// ProbeURLEnvPrefix + a probe's EnvKey overrides that probe's URL, e.g.
	// CANARY_PROBE_URL_YOUTUBE_SHORT.
	ProbeURLEnvPrefix = "CANARY_PROBE_URL_"
	// ProbeMediaCountEnvPrefix + a probe's EnvKey overrides how many assets
	// that probe's post is expected to have, e.g.
	// CANARY_PROBE_MEDIA_COUNT_REDDIT_GALLERY=6. Needed when a rotated probe
	// URL points at a post with a different number of assets than the built-in
	// one; without it the completeness check would keep asserting the old
	// count.
	ProbeMediaCountEnvPrefix = "CANARY_PROBE_MEDIA_COUNT_"

	// MinSchedule is the shortest accepted interval. Canaries archive real
	// media from real platforms; running them every few minutes would be
	// indistinguishable from abuse and would get Arker's IP throttled, which is
	// a strictly worse outcome than a slightly stale health signal.
	MinSchedule = 15 * time.Minute

	// DefaultProbeTimeout bounds a single probe so one wedged extractor cannot
	// hold the canary queue open indefinitely.
	DefaultProbeTimeout = 15 * time.Minute
)

// Config is the canary runtime configuration. The zero value is a valid,
// fully disabled configuration: no schedule, no paid probes, zero budget.
type Config struct {
	// Schedule is the raw CANARY_SCHEDULE value ("" / "off" = disabled,
	// otherwise a Go duration such as "6h").
	Schedule string
	// Interval is the parsed schedule. Zero means disabled.
	Interval time.Duration
	// AllowPaidFallback lets probes reach the Bright Data fallback. It is off
	// by default and is NOT part of turning canaries on: enabling the schedule
	// buys a free native-only health signal and nothing else.
	AllowPaidFallback bool
	// MaxCostPerRunUSD and MaxCostPerDayUSD cap paid probes. They only matter
	// when AllowPaidFallback is true; with it false, the runner refuses to hold
	// a paid archiver at all and the ceilings are unreachable by construction.
	MaxCostPerRunUSD float64
	MaxCostPerDayUSD float64
	// ProbeKeys selects probes explicitly ("youtube/video,vimeo/video").
	// Empty means "every default-enabled probe".
	ProbeKeys []string
	// ProbeURLOverrides maps a probe EnvKey to a replacement URL.
	ProbeURLOverrides map[string]string
	// ProbeMediaCountOverrides maps a probe EnvKey to a replacement expected
	// asset count.
	ProbeMediaCountOverrides map[string]int
	// ProbeTimeout bounds one probe.
	ProbeTimeout time.Duration
}

// ScheduleEnabled reports whether recurring canaries should be registered.
func (c Config) ScheduleEnabled() bool { return c.Interval > 0 }

// EffectiveProbeTimeout falls back to the default when unset.
func (c Config) EffectiveProbeTimeout() time.Duration {
	if c.ProbeTimeout > 0 {
		return c.ProbeTimeout
	}
	return DefaultProbeTimeout
}

// RawConfig is the unparsed environment shape, filled by envconfig in cmd.
type RawConfig struct {
	Schedule          string        `envconfig:"CANARY_SCHEDULE"`
	AllowPaidFallback bool          `envconfig:"CANARY_ALLOW_PAID_FALLBACK" default:"false"`
	MaxCostPerRunUSD  float64       `envconfig:"CANARY_MAX_COST_USD_PER_RUN" default:"0.25"`
	MaxCostPerDayUSD  float64       `envconfig:"CANARY_MAX_COST_USD_PER_DAY" default:"1.00"`
	Probes            string        `envconfig:"CANARY_PROBES"`
	ProbeTimeout      time.Duration `envconfig:"CANARY_PROBE_TIMEOUT" default:"15m"`
}

// LoadConfig parses the raw environment shape plus the process environment
// (for the per-probe URL overrides, which are dynamic and so cannot be
// expressed as envconfig struct fields).
//
// Parsing never returns a fatally broken config: an unusable schedule yields a
// disabled config and an error. Arker deliberately does not die over
// misconfigured optional subsystems — an unreadable cookie file once took
// production down — and a typo'd canary interval must not be able to keep the
// archiver from starting.
func LoadConfig(raw RawConfig, environ []string) (Config, error) {
	cfg := Config{
		Schedule:                 strings.TrimSpace(raw.Schedule),
		AllowPaidFallback:        raw.AllowPaidFallback,
		MaxCostPerRunUSD:         raw.MaxCostPerRunUSD,
		MaxCostPerDayUSD:         raw.MaxCostPerDayUSD,
		ProbeKeys:                parseProbeKeys(raw.Probes),
		ProbeURLOverrides:        parsePrefixedEnv(environ, ProbeURLEnvPrefix),
		ProbeMediaCountOverrides: parseMediaCountOverrides(environ),
		ProbeTimeout:             raw.ProbeTimeout,
	}

	interval, err := ParseSchedule(cfg.Schedule)
	if err != nil {
		return cfg, err
	}
	cfg.Interval = interval
	return cfg, nil
}

// ParseSchedule turns a CANARY_SCHEDULE value into an interval. Empty, "off",
// "disabled", "none", and "0" all mean disabled, so an operator can turn
// canaries off by neutering the value as well as by unsetting it.
func ParseSchedule(value string) (time.Duration, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "", "off", "disabled", "none", "0":
		return 0, nil
	}
	interval, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: expected a Go duration such as \"6h\" (or \"off\"): %w", ScheduleEnvVar, value, err)
	}
	if interval < MinSchedule {
		return 0, fmt.Errorf("invalid %s %q: minimum interval is %s (probes archive real media from real platforms)", ScheduleEnvVar, value, MinSchedule)
	}
	return interval, nil
}

func parseProbeKeys(value string) []string {
	parts := strings.Split(value, ",")
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.ToLower(strings.TrimSpace(part))
		if key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

func parsePrefixedEnv(environ []string, prefix string) map[string]string {
	overrides := map[string]string{}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		key := strings.TrimPrefix(name, prefix)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		overrides[key] = value
	}
	return overrides
}

// parseMediaCountOverrides ignores values that are not positive integers: a
// typo must not silently disable a probe's completeness check.
func parseMediaCountOverrides(environ []string) map[string]int {
	overrides := map[string]int{}
	for key, value := range parsePrefixedEnv(environ, ProbeMediaCountEnvPrefix) {
		count, err := strconv.Atoi(value)
		if err != nil || count <= 0 {
			continue
		}
		overrides[key] = count
	}
	return overrides
}
