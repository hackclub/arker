package canary

import (
	"strings"
	"testing"

	"arker/internal/utils"
)

// The scheduled probe set must be free, anonymous, and cookie-free. This is the
// test that stops a future edit from quietly adding an Instagram probe to the
// default sweep.
func TestDefaultProbeSetIsAnonymousAndFree(t *testing.T) {
	selected, problems := SelectProbes(Config{}, DefaultProbes(), false)
	if len(problems) != 0 {
		t.Fatalf("default selection reported problems: %v", problems)
	}
	if len(selected) == 0 {
		t.Fatal("no probes are enabled by default")
	}
	for _, probe := range selected {
		if probe.RequiresCookies {
			t.Errorf("probe %s requires cookies but is enabled by default", probe.Key())
		}
		if strings.Contains(strings.ToLower(probe.URL), "instagram.com") {
			t.Errorf("probe %s targets Instagram and is enabled by default", probe.Key())
		}
		if probe.MinMediaBytes <= 0 {
			t.Errorf("probe %s has no media floor, so an empty artifact would pass", probe.Key())
		}
		if probe.ExpectedType != utils.ArchiveTypeYtDlp && probe.ExpectedType != utils.ArchiveTypeGalleryDl {
			t.Errorf("probe %s expects %q, which is not a media archiver", probe.Key(), probe.ExpectedType)
		}
	}
}

func TestNoInstagramProbeIsDefaultEnabled(t *testing.T) {
	for _, probe := range DefaultProbes() {
		if probe.Platform != "instagram" {
			continue
		}
		if probe.DefaultEnabled {
			t.Errorf("Instagram probe %s is enabled by default", probe.Key())
		}
		if !probe.RequiresCookies {
			t.Errorf("Instagram probe %s is not marked cookies-required", probe.Key())
		}
	}
}

// Explicitly selecting a cookies-required probe with no cookie jar drops it
// with an explanation rather than scheduling a guaranteed failure.
func TestSelectProbesSkipsCookieProbesWithoutCookies(t *testing.T) {
	cfg := Config{ProbeKeys: []string{"instagram/reel"}}

	selected, problems := SelectProbes(cfg, DefaultProbes(), false)
	if len(selected) != 0 {
		t.Fatalf("selected %d probes, want none without cookies", len(selected))
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "requires media cookies") {
		t.Fatalf("problems = %v, want one cookies explanation", problems)
	}

	withCookies, problems := SelectProbes(cfg, DefaultProbes(), true)
	if len(withCookies) != 1 || len(problems) != 0 {
		t.Fatalf("with cookies: selected %d probes, problems %v", len(withCookies), problems)
	}
}

func TestSelectProbesAppliesURLOverride(t *testing.T) {
	cfg := Config{
		ProbeKeys:         []string{"youtube/video"},
		ProbeURLOverrides: map[string]string{"YOUTUBE_VIDEO": "https://www.youtube.com/watch?v=REPLACED"},
	}
	selected, problems := SelectProbes(cfg, DefaultProbes(), false)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(selected) != 1 || selected[0].URL != "https://www.youtube.com/watch?v=REPLACED" {
		t.Fatalf("override not applied: %+v", selected)
	}
}

func TestSelectProbesReportsUnknownKey(t *testing.T) {
	cfg := Config{ProbeKeys: []string{"myspace/post"}}
	selected, problems := SelectProbes(cfg, DefaultProbes(), false)
	if len(selected) != 0 {
		t.Fatalf("selected %d probes for an unknown key", len(selected))
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "unknown canary probe") {
		t.Fatalf("problems = %v, want an unknown-probe error", problems)
	}
}

func TestFilterByPlatform(t *testing.T) {
	probes, _ := SelectProbes(Config{}, DefaultProbes(), false)

	if got := FilterByPlatform(probes, ""); len(got) != len(probes) {
		t.Errorf("empty filter returned %d of %d probes", len(got), len(probes))
	}
	youtube := FilterByPlatform(probes, "youtube")
	if len(youtube) != 2 {
		t.Errorf("youtube filter returned %d probes, want 2 (video + short)", len(youtube))
	}
	single := FilterByPlatform(probes, "youtube/short")
	if len(single) != 1 || single[0].PostType != "short" {
		t.Errorf("probe-key filter returned %+v, want just youtube/short", single)
	}
	if got := FilterByPlatform(probes, "myspace"); len(got) != 0 {
		t.Errorf("unknown platform returned %d probes", len(got))
	}
}

func TestProbeEnvKey(t *testing.T) {
	probe := Probe{Platform: "youtube", PostType: "short"}
	if got := probe.EnvKey(); got != "YOUTUBE_SHORT" {
		t.Errorf("EnvKey = %q, want YOUTUBE_SHORT", got)
	}
	if got := (Probe{Platform: "x", PostType: "status-post"}).EnvKey(); got != "X_STATUS_POST" {
		t.Errorf("EnvKey = %q, want X_STATUS_POST", got)
	}
}

// Probe keys have to be unique: the health view is one row per
// platform/post-type, so a duplicate would make two probes overwrite each
// other's verdict.
func TestCatalogKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, probe := range DefaultProbes() {
		if seen[probe.Key()] {
			t.Errorf("duplicate probe key %s", probe.Key())
		}
		seen[probe.Key()] = true
	}
}
