package utils

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	galleryDlUserAgentMu sync.RWMutex
	galleryDlUserAgent   string

	galleryDlSleepRequestMu sync.RWMutex
	galleryDlSleepRequest   string
)

// GalleryDlVersion returns the installed gallery-dl version for archive logs.
// Like yt-dlp, gallery-dl's extractors break when sites change and are fixed
// upstream quickly, so recording the version per job makes a stale image
// obvious the moment a site starts failing.
func GalleryDlVersion(ctx context.Context) (string, error) {
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(versionCtx, "gallery-dl", "--version").Output()
	if versionCtx.Err() != nil {
		return "", versionCtx.Err()
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// InitGalleryDlUserAgent configures an optional User-Agent override for
// gallery-dl. Leave it empty in normal operation: gallery-dl sets a per-site
// User-Agent already (the Instagram extractor, for example, sends a current
// Chrome UA because Instagram serves lower-quality video to anything else), and
// a global override would replace those tuned defaults everywhere.
func InitGalleryDlUserAgent(userAgent string) string {
	value := strings.TrimSpace(userAgent)
	galleryDlUserAgentMu.Lock()
	galleryDlUserAgent = value
	galleryDlUserAgentMu.Unlock()
	return value
}

// GalleryDlUserAgentArgs returns --user-agent arguments, or nil when no
// override is configured.
func GalleryDlUserAgentArgs() []string {
	galleryDlUserAgentMu.RLock()
	defer galleryDlUserAgentMu.RUnlock()
	if galleryDlUserAgent == "" {
		return nil
	}
	return []string{"--user-agent", galleryDlUserAgent}
}

// InitGalleryDlSleepRequest configures an optional delay between gallery-dl's
// HTTP requests, accepting a number ("1") or a range ("0.5-1.5") of seconds.
//
// Leave it unset. gallery-dl already ships a per-site request interval tuned to
// what each site tolerates — Instagram's extractor waits a randomized 6 to 12
// seconds between API calls, which is precisely the politeness that keeps this
// account un-blocked. --sleep-request is a *root* config key, and gallery-dl
// resolves root keys ahead of per-extractor ones, so setting it here does not
// add a floor: it replaces those tuned defaults everywhere, and any value below
// Instagram's would make bulk archiving more likely to trip a soft block, not
// less. Set it only to slow gallery-dl down further.
func InitGalleryDlSleepRequest(sleepRequest string) string {
	value := strings.TrimSpace(sleepRequest)
	galleryDlSleepRequestMu.Lock()
	galleryDlSleepRequest = value
	galleryDlSleepRequestMu.Unlock()
	return value
}

// GalleryDlSleepArgs returns --sleep-request arguments, or nil when no override
// is configured (the normal case, leaving gallery-dl's per-site intervals in
// force).
func GalleryDlSleepArgs() []string {
	galleryDlSleepRequestMu.RLock()
	defer galleryDlSleepRequestMu.RUnlock()
	if galleryDlSleepRequest == "" {
		return nil
	}
	return []string{"--sleep-request", galleryDlSleepRequest}
}
