// Package canary runs recurring production probes against the social archive
// contract.
//
// A probe archives a known-good public post through the production archive
// path and then checks the whole contract on the result: the item completed,
// real media bytes of the right kind landed in storage, normalized metadata is
// present and sane, the raw provider record is retrievable, and the artifact
// came from the free native path. Anything less is a failure with a named
// stage and reason.
//
// Two properties are load-bearing:
//
//   - Canaries cost nothing. They run with an archiver map that has no paid
//     fallback wired into it, so a probe cannot spend money even if every
//     native extractor breaks at once. A native failure that a paid fallback
//     would have rescued in production is still reported here as a native-path
//     failure, because that is what it is.
//   - Canaries are off until an operator turns them on. Nothing recurring runs
//     without CANARY_SCHEDULE set.
package canary

import (
	"fmt"
	"sort"
	"strings"

	"arker/internal/utils"
)

// Media kinds a probe expects to find in the stored artifact.
const (
	MediaKindVideo = "video"
	MediaKindImage = "image"
)

// Probe is one platform/post-type slot: a stable public URL plus what a
// healthy archive of it must look like.
type Probe struct {
	// Platform and PostType name the slot. Key is "platform/post_type" and is
	// the identifier used in config, admin output, and the canary_runs table.
	Platform string
	PostType string
	// URL is the probe target. Overridable per slot via
	// CANARY_PROBE_URL_<PLATFORM>_<POST_TYPE> so an operator can rotate a dead
	// probe without a deploy.
	URL string
	// ExpectedType is the archive type this URL must route to. Checked before
	// anything is archived: if routing changes underneath us, that is itself a
	// contract failure and it is cheaper to catch it here than after a download.
	ExpectedType string
	// ExpectedMedia is the media kind the stored artifact must contain.
	ExpectedMedia string
	// MinMediaBytes rejects a "successful" archive that stored a placeholder,
	// an error page, or a 200-byte thumbnail instead of the real asset.
	MinMediaBytes int64
	// RequiresCookies marks probes against sites that serve logged-out clients
	// nothing. They can never be enabled by default: without a cookie jar they
	// fail for a reason that says nothing about the platform's health, and with
	// one they spend a real account's reputation on unattended traffic.
	RequiresCookies bool
	// DefaultEnabled is whether a scheduled sweep includes this probe when the
	// operator has not listed probes explicitly.
	DefaultEnabled bool
	// Note documents why this URL was chosen and what to watch for. It is
	// surfaced in the admin payload so whoever reads a failure at 3am sees the
	// rotation guidance without opening the runbook.
	Note string
}

// Key returns the probe's stable identifier, "platform/post_type".
func (p Probe) Key() string { return p.Platform + "/" + p.PostType }

// EnvKey returns the environment-variable suffix used to override this probe's
// URL: youtube/short -> YOUTUBE_SHORT.
func (p Probe) EnvKey() string {
	replacer := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	return strings.ToUpper(replacer.Replace(p.Key()))
}

// defaultProbes is the probe catalog.
//
// Every default-enabled entry is anonymous-safe, free, and points at a post
// chosen for permanence rather than for being interesting: an institutional
// account, a historically significant upload, or something old enough and
// linked-to enough that deleting it would be news. Probe URLs are still
// expected to rot — see docs/canaries.md for rotation guidance and the
// per-probe override variables.
//
// Instagram is present but never default-enabled, and is marked
// RequiresCookies: an unattended Instagram probe either fails meaninglessly
// (logged out) or burns a real session against a platform that rate-limits
// aggressively. TikTok is present and default-disabled for the same category
// of reason: its anonymous extraction is hostile enough that a failing probe
// would mostly measure TikTok's bot defenses, not Arker.
var defaultProbes = []Probe{
	{
		Platform: "youtube", PostType: "video",
		URL:            "https://www.youtube.com/watch?v=jNQXAC9IVRw",
		ExpectedType:   utils.ArchiveTypeYtDlp,
		ExpectedMedia:  MediaKindVideo,
		MinMediaBytes:  200 * 1024,
		DefaultEnabled: true,
		Note:           "\"Me at the zoo\", the first YouTube video (18s, ~500KB). Permanent as long as YouTube is: it is the platform's own origin artifact.",
	},
	{
		Platform: "youtube", PostType: "short",
		URL:            "https://www.youtube.com/shorts/kL9E5Y1zHrY",
		ExpectedType:   utils.ArchiveTypeYtDlp,
		ExpectedMedia:  MediaKindVideo,
		MinMediaBytes:  200 * 1024,
		DefaultEnabled: true,
		Note:           "A NASA Short. Shorts are a distinct extraction path (vertical formats, /shorts/ URL shape), so this slot must point at a real Short, not a regular video ID: YouTube redirects /shorts/<non-short-id> to /watch and the probe would silently stop testing Shorts. Rotate with CANARY_PROBE_URL_YOUTUBE_SHORT.",
	},
	{
		Platform: "vimeo", PostType: "video",
		URL:            "https://vimeo.com/76979871",
		ExpectedType:   utils.ArchiveTypeYtDlp,
		ExpectedMedia:  MediaKindVideo,
		MinMediaBytes:  200 * 1024,
		DefaultEnabled: true,
		Note:           "Vimeo's own staff-hosted demo video. Vimeo is the non-YouTube video path; keeping it green proves yt-dlp coverage is not YouTube-specific.",
	},
	{
		Platform: "reddit", PostType: "gallery",
		URL:            "https://www.reddit.com/r/pics/comments/haucpf/",
		ExpectedType:   utils.ArchiveTypeGalleryDl,
		ExpectedMedia:  MediaKindImage,
		MinMediaBytes:  20 * 1024,
		DefaultEnabled: true,
		Note:           "A multi-image Reddit gallery post. Galleries are the multi-asset completeness case: a partial download here is exactly the false-green this canary exists to catch.",
	},
	{
		Platform: "bluesky", PostType: "post",
		URL:            "https://bsky.app/profile/bsky.app/post/3kkrn6mgvpk2j",
		ExpectedType:   utils.ArchiveTypeGalleryDl,
		ExpectedMedia:  MediaKindImage,
		MinMediaBytes:  10 * 1024,
		DefaultEnabled: true,
		Note:           "An official @bsky.app post with an image embed. Bluesky media lives behind blob CDN URLs, so this probe checks blob retrieval, not just post JSON.",
	},
	{
		Platform: "imgur", PostType: "album",
		URL:            "https://imgur.com/a/nBIkD",
		ExpectedType:   utils.ArchiveTypeGalleryDl,
		ExpectedMedia:  MediaKindImage,
		MinMediaBytes:  20 * 1024,
		DefaultEnabled: true,
		Note:           "A public multi-image Imgur album. Imgur purged large amounts of old anonymous content in 2023, so this is the slot most likely to need rotation; verify it manually before activation.",
	},
	{
		Platform: "tiktok", PostType: "video",
		URL:            "https://www.tiktok.com/@nasa/video/7095021962566440750",
		ExpectedType:   utils.ArchiveTypeYtDlp,
		ExpectedMedia:  MediaKindVideo,
		MinMediaBytes:  200 * 1024,
		DefaultEnabled: false,
		Note:           "Default-disabled. TikTok blocks datacenter IPs aggressively and its extractor breaks often; scheduled failures here would mostly report TikTok's bot defenses rather than an Arker regression. Enable deliberately via CANARY_PROBES if you want that signal anyway.",
	},
	{
		Platform: "instagram", PostType: "reel",
		URL:             "https://www.instagram.com/reel/C0000000000/",
		ExpectedType:    utils.ArchiveTypeYtDlp,
		ExpectedMedia:   MediaKindVideo,
		MinMediaBytes:   200 * 1024,
		RequiresCookies: true,
		DefaultEnabled:  false,
		Note:            "Default-disabled and cookies-required. Instagram serves logged-out clients nothing, so this probe is meaningless without a cookie jar and risky with one (unattended traffic against a real session). The URL is a placeholder: set CANARY_PROBE_URL_INSTAGRAM_REEL before enabling.",
	},
	{
		Platform: "instagram", PostType: "carousel",
		URL:             "https://www.instagram.com/p/C0000000000/",
		ExpectedType:    utils.ArchiveTypeGalleryDl,
		ExpectedMedia:   MediaKindImage,
		MinMediaBytes:   20 * 1024,
		RequiresCookies: true,
		DefaultEnabled:  false,
		Note:            "Default-disabled and cookies-required, same reasoning as instagram/reel. The URL is a placeholder: set CANARY_PROBE_URL_INSTAGRAM_CAROUSEL before enabling.",
	},
}

// DefaultProbes returns a copy of the probe catalog.
func DefaultProbes() []Probe {
	out := make([]Probe, len(defaultProbes))
	copy(out, defaultProbes)
	return out
}

// SelectProbes resolves the catalog against config: URL overrides applied,
// selection applied, and probes that must not run silently removed.
//
// A cookies-required probe that is selected without a cookie jar configured is
// dropped with an explanatory error rather than run: failing it would report a
// missing credential as a platform outage every six hours.
func SelectProbes(cfg Config, catalog []Probe, cookiesConfigured bool) ([]Probe, []error) {
	var problems []error

	byKey := make(map[string]Probe, len(catalog))
	keys := make([]string, 0, len(catalog))
	for _, probe := range catalog {
		if override, ok := cfg.ProbeURLOverrides[probe.EnvKey()]; ok && override != "" {
			probe.URL = override
		}
		byKey[probe.Key()] = probe
		keys = append(keys, probe.Key())
	}

	selected := make([]string, 0, len(keys))
	if len(cfg.ProbeKeys) > 0 {
		for _, key := range cfg.ProbeKeys {
			if _, ok := byKey[key]; !ok {
				problems = append(problems, fmt.Errorf("unknown canary probe %q (known: %s)", key, strings.Join(sortedKeys(byKey), ", ")))
				continue
			}
			selected = append(selected, key)
		}
	} else {
		for _, key := range keys {
			if byKey[key].DefaultEnabled {
				selected = append(selected, key)
			}
		}
	}

	out := make([]Probe, 0, len(selected))
	for _, key := range selected {
		probe := byKey[key]
		if probe.RequiresCookies && !cookiesConfigured {
			problems = append(problems, fmt.Errorf("canary probe %q requires media cookies and none are configured; skipping it (a logged-out failure would report a missing credential as a platform outage)", key))
			continue
		}
		out = append(out, probe)
	}
	return out, problems
}

func sortedKeys(m map[string]Probe) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// FilterByPlatform narrows a probe set to one platform, for
// POST /admin/canaries/run?platform=youtube. An empty platform keeps all.
func FilterByPlatform(probes []Probe, platform string) []Probe {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		return probes
	}
	out := make([]Probe, 0, len(probes))
	for _, probe := range probes {
		if strings.ToLower(probe.Platform) == platform || strings.ToLower(probe.Key()) == platform {
			out = append(out, probe)
		}
	}
	return out
}
