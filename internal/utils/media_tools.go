package utils

import (
	"strings"
	"sync/atomic"
)

// Shared configuration for the external media download tools Arker shells out
// to. yt-dlp and gallery-dl are the video and image halves of the same job and
// take the same authentication and proxy flags, so one configured cookie jar
// and one configured proxy serve both. These wrappers exist so an archiver can
// say what it means ("media cookies") instead of borrowing a yt-dlp-specific
// name for a gallery-dl invocation.

// MediaCookieArgsForRun returns --cookies arguments pointing at a fresh private
// copy of the configured cookie jar, plus a cleanup function to call once the
// tool exits. Both yt-dlp and gallery-dl accept a Netscape cookies.txt via
// --cookies. Returns no arguments when no cookies are configured.
func MediaCookieArgsForRun() ([]string, func(), error) {
	return YtDlpCookieArgsForRun()
}

// MediaProxyArgs returns --proxy arguments for a media tool invocation, or nil
// when no proxy is configured. Both tools spell the flag --proxy and accept
// http/https/socks5 URLs.
func MediaProxyArgs() []string {
	return YtDlpProxyArgs()
}

// MediaCookiesConfigured reports whether a cookie jar is available. Sites that
// serve nothing to logged-out clients are not worth queueing without one: the
// job cannot succeed, and the requests it spends still count against the rate
// limits of the sites that can.
func MediaCookiesConfigured() bool {
	ytDlpCookiesMu.RLock()
	defer ytDlpCookiesMu.RUnlock()
	return ytDlpCookiesFilePath != ""
}

// brightDataMediaFallback holds the configured Bright Data client's own
// answer to "could you rescue this URL for this archive type". It lives here
// rather than in the brightdata package because URL routing (which archive
// items to create) is a utils concern and must not import the archiver stack.
var brightDataMediaFallback atomic.Value // of mediaFallbackSupport

type mediaFallbackSupport struct {
	supports func(rawURL, itemType string) bool
}

// SetBrightDataMediaFallback is called once at startup after the Bright Data
// client is (or is not) constructed. A nil predicate disables the fallback.
//
// It takes a predicate rather than a boolean because routing has to ask a
// sharper question than "is the fallback switched on": a login-only site is
// only worth queueing when the fallback covers that exact site and archive
// type. Listing those sites here instead would be a second copy of the
// coverage table, free to drift from the one the client actually implements —
// and the failure mode of drift is a guaranteed-failed archive item, or a
// silently missing one.
func SetBrightDataMediaFallback(supports func(rawURL, itemType string) bool) {
	brightDataMediaFallback.Store(mediaFallbackSupport{supports: supports})
}

// BrightDataCanRescue reports whether the configured fallback covers this URL
// and archive type. An unconfigured fallback covers nothing, which is also the
// zero value's answer.
func BrightDataCanRescue(rawURL, itemType string) bool {
	support, _ := brightDataMediaFallback.Load().(mediaFallbackSupport)
	return support.supports != nil && support.supports(rawURL, itemType)
}

// MediaProxyRedactionSecrets returns substrings that must never reach persisted
// logs, currently the proxy URL (which may embed credentials).
func MediaProxyRedactionSecrets() []string {
	return YtDlpProxyRedactionSecrets()
}

// TruncateForLog shortens s to at most limit runes, appending an ellipsis when
// it had to cut. Archive logs are chunked into 1KB database rows, so a long
// post caption is worth summarizing rather than streaming in full.
func TruncateForLog(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
