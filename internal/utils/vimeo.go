package utils

import (
	"net/url"
	"strings"
)

// vimeoPlayerHost serves Vimeo's embeddable player. Unlike the main site it
// still answers logged-out requests, which is the only reason Vimeo archiving
// works at all — see VimeoPlayerURL.
const vimeoPlayerHost = "player.vimeo.com"

// vimeoNonVideoPrefixes are first path segments that are not a plain video
// page. An event is a livestream with its own embed shape and an on-demand
// page is a storefront, so rewriting either to /video/<id> would fetch the
// wrong thing (or nothing).
var vimeoNonVideoPrefixes = map[string]bool{
	"event":    true,
	"ondemand": true,
}

// VimeoPlayerURL rewrites a Vimeo main-site video URL to the equivalent player
// URL, reporting whether it rewrote anything.
//
// yt-dlp 2026.08.04 refuses main-site Vimeo URLs outright: "The Vimeo extractor
// only works when logged-in" (verified 2026-08-12 against vimeo.com/76979871,
// which fails, while player.vimeo.com/video/76979871 returns title, id and
// duration anonymously). Arker has no Vimeo account, so without this rewrite
// every Vimeo capture is a guaranteed failure.
//
// Only the fetch target changes. The capture keeps the URL the user submitted
// as its identity, and a URL that is not a plain video page is left alone.
//
// Shapes handled:
//
//	vimeo.com/76979871                     -> player.vimeo.com/video/76979871
//	vimeo.com/76979871/a1b2c3d4e5          -> player.vimeo.com/video/76979871?h=a1b2c3d4e5
//	vimeo.com/76979871?h=a1b2c3d4e5        -> player.vimeo.com/video/76979871?h=a1b2c3d4e5
//	vimeo.com/channels/staffpicks/76979871 -> player.vimeo.com/video/76979871
//	vimeo.com/groups/x/videos/76979871     -> player.vimeo.com/video/76979871
//	player.vimeo.com/video/76979871        -> unchanged
//	vimeo.com/user12345, /ondemand/..., /event/... -> unchanged
//
// The trailing hash is Vimeo's unlisted-video token; dropping it would turn a
// private link into a 404.
func VimeoPlayerURL(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, false
	}
	hostname := strings.ToLower(parsed.Hostname())
	// Already a player URL: nothing to rewrite.
	if hostname == vimeoPlayerHost || !hostMatches(hostname, "vimeo.com") {
		return rawURL, false
	}

	segments := splitPathSegments(parsed.Path)
	if len(segments) == 0 || vimeoNonVideoPrefixes[strings.ToLower(segments[0])] {
		return rawURL, false
	}

	videoID, hash := vimeoIDAndHash(segments)
	if videoID == "" {
		return rawURL, false
	}
	if hash == "" {
		hash = parsed.Query().Get("h")
	}

	player := &url.URL{Scheme: "https", Host: vimeoPlayerHost, Path: "/video/" + videoID}
	if hash != "" {
		player.RawQuery = url.Values{"h": []string{hash}}.Encode()
	}
	return player.String(), true
}

// vimeoIDAndHash picks the video ID (and unlisted hash, when present) out of a
// Vimeo path.
//
// The ID is the last all-numeric segment, which is what every container shape
// (/channels/x/<id>, /groups/x/videos/<id>, /album/x/video/<id>) has in common.
// The one exception is /<id>/<hash> with an all-hex-digit hash: two bare
// numeric segments can only be that shape, so the first one is the ID.
func vimeoIDAndHash(segments []string) (videoID, hash string) {
	if len(segments) == 2 && isAllDigits(segments[0]) && isAllDigits(segments[1]) {
		return segments[0], segments[1]
	}

	for i := len(segments) - 1; i >= 0; i-- {
		if !isAllDigits(segments[i]) {
			continue
		}
		if i+1 < len(segments) && isVimeoHash(segments[i+1]) {
			return segments[i], segments[i+1]
		}
		return segments[i], ""
	}
	return "", ""
}

// isVimeoHash reports whether a path segment looks like an unlisted-video
// token: a short alphanumeric string, never a word with punctuation in it.
func isVimeoHash(segment string) bool {
	if len(segment) < 4 || len(segment) > 32 {
		return false
	}
	for _, r := range segment {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func isAllDigits(segment string) bool {
	if segment == "" {
		return false
	}
	for _, r := range segment {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func splitPathSegments(path string) []string {
	var segments []string
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

// YtDlpFetchURL returns the URL yt-dlp should actually fetch for rawURL.
//
// It is normally the URL itself. A platform only earns an entry here when the
// page a human visits is not the page yt-dlp can read — currently just Vimeo,
// whose main site is login-only. The archive is still recorded against the
// original URL: this changes where the bytes come from, not what was archived.
func YtDlpFetchURL(rawURL string) string {
	if player, rewritten := VimeoPlayerURL(rawURL); rewritten {
		return player
	}
	return rawURL
}

// YtDlpRefererArgsForURL returns the --referer arguments a fetch URL needs, or
// nil. Vimeo's player serves embeds, so it is entitled to ask who is embedding
// it; sending its own site keeps the request looking like an ordinary embed.
func YtDlpRefererArgsForURL(fetchURL string) []string {
	parsed, err := url.Parse(fetchURL)
	if err != nil {
		return nil
	}
	if strings.ToLower(parsed.Hostname()) == vimeoPlayerHost {
		return []string{"--referer", "https://vimeo.com/"}
	}
	return nil
}
