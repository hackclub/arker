package utils

import (
	"arker/internal/models"
	"crypto/rand"
	"fmt"
	"gorm.io/gorm"
	"math/big"
	"net/url"
	"strings"
)

// Extract repository name from git URL
func ExtractRepoName(url string) string {
	// Remove .git suffix if present
	url = strings.TrimSuffix(url, ".git")

	// Extract last part of path
	parts := strings.Split(strings.TrimRight(url, "/"), "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "repo"
}

// Check if URL is a git repository
func IsGitURL(url string) bool {
	lowerURL := strings.ToLower(url)

	// Direct .git URLs
	if strings.HasSuffix(lowerURL, ".git") {
		return true
	}

	// URLs containing git subdomain
	if strings.Contains(lowerURL, "git.") {
		return true
	}

	// Check for repository URLs on hosting platforms
	// These require at least username/reponame format
	platforms := []string{
		"github.com/",
		"gitlab.com/",
		"bitbucket.org/",
		"codeberg.org/",
	}

	for _, platform := range platforms {
		if strings.Contains(lowerURL, platform) {
			// Extract path after platform
			parts := strings.Split(lowerURL, platform)
			if len(parts) > 1 {
				path := strings.Trim(parts[1], "/")
				pathSegments := strings.Split(path, "/")

				// Must have at least username/reponame (2 segments)
				// Exclude common non-repository paths
				if len(pathSegments) >= 2 && !isNonRepoPath(pathSegments) {
					return true
				}
			}
		}
	}

	return false
}

// Check if path segments indicate a non-repository URL
func isNonRepoPath(segments []string) bool {
	if len(segments) == 0 {
		return true
	}

	// Common non-repository paths on GitHub/GitLab
	nonRepoPaths := []string{
		"settings", "notifications", "explore", "marketplace",
		"pricing", "features", "security", "enterprise",
		"login", "join", "new", "organizations", "teams",
		"dashboard", "pulls", "issues", "search", "trending",
		"collections", "events", "sponsors", "about",
	}

	// If first segment is a non-repo path, it's not a repository
	for _, nonRepo := range nonRepoPaths {
		if segments[0] == nonRepo {
			return true
		}
	}

	// If only one segment (just username), it's a profile page
	if len(segments) == 1 {
		return true
	}

	return false
}

// Check if URL is a YouTube URL
func IsYouTubeURL(url string) bool {
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, "youtube.com") || strings.Contains(lowerURL, "youtu.be")
}

// Check if URL is a Vimeo URL
func IsVimeoURL(url string) bool {
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, "vimeo.com")
}

// Check if URL is an Instagram URL
func IsInstagramURL(url string) bool {
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, "instagram.com") && (strings.Contains(lowerURL, "/reel/") || strings.Contains(lowerURL, "/p/") || strings.Contains(lowerURL, "/tv/"))
}

// IsInstagramPhotoPostURL reports whether an Instagram URL is a feed post
// (/p/ or /tv/) rather than a reel.
//
// Feed posts are usually photos or mixed photo/video carousels, which yt-dlp
// cannot download at all — it fails with "There is no video in this post". They
// go to gallery-dl instead, which fetches every slide plus the caption and post
// metadata, and picks up any video slides on the way. Reels are always a single
// video, which is exactly yt-dlp's job, so they stay on that path.
func IsInstagramPhotoPostURL(url string) bool {
	if !IsInstagramURL(url) {
		return false
	}
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, "/p/") || strings.Contains(lowerURL, "/tv/")
}

// Check if URL is a TikTok video URL (full video links and vm/vt/t short links)
//
// Photo posts (/photo/) are deliberately excluded: yt-dlp cannot download them
// at all. They go to gallery-dl instead — see IsTikTokPhotoPostURL.
func IsTikTokURL(url string) bool {
	lowerURL := strings.ToLower(url)
	if strings.Contains(lowerURL, "vm.tiktok.com/") || strings.Contains(lowerURL, "vt.tiktok.com/") {
		return true
	}
	if !strings.Contains(lowerURL, "tiktok.com") {
		return false
	}
	return strings.Contains(lowerURL, "/video/") ||
		strings.Contains(lowerURL, "tiktok.com/t/")
}

// IsTikTokPhotoPostURL reports whether a TikTok URL is a photo post
// (tiktok.com/@user/photo/<id> or tiktok.com/share/photo/<id>).
//
// A TikTok photo post is a slideshow of stills with a music track, not a
// video: yt-dlp rejects it, and before this existed the URL matched no media
// archiver at all, so it silently became an MHTML/screenshot-only capture.
// gallery-dl's tiktok extractor handles both shapes (gallery-dl 1.32.9
// extractor/tiktok.py TiktokPostExtractor, pattern
// "/(?:@([\w_.-]*)|share)/(?:phot|vide)o/(\d+)") and downloads each still from
// its CDN URL plus the post's audio track, with no yt-dlp module involved.
//
// Short links (vm./vt./tiktok.com/t/) are NOT matched here — see
// IsTikTokShortLinkURL for why they stay on yt-dlp.
func IsTikTokPhotoPostURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !hostMatches(strings.ToLower(parsed.Hostname()), "tiktok.com") {
		return false
	}
	return strings.Contains(strings.ToLower(parsed.Path), "/photo/")
}

// IsTikTokShortLinkURL reports whether a URL is a TikTok share short link
// (vm.tiktok.com/X, vt.tiktok.com/X, tiktok.com/t/X).
//
// A short link hides its post type behind a redirect, so routing cannot tell a
// video from a photo post without resolving it. Arker keeps short links on
// yt-dlp: the overwhelming majority are videos, and resolving the redirect
// would mean a network call inside GetArchiveTypes, which is a pure function
// called from request handlers and the queue.
//
// The photo-post case therefore fails, but it fails EXPLICITLY: yt-dlp reports
// that the post has no video, the yt-dlp item goes to failed, and
// IsSocialMediaPostURL still recognizes the URL, so the API returns a failed
// social_post rather than a silent MHTML-only success. See
// docs note in IsSocialMediaPostURL.
func IsTikTokShortLinkURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "vm.tiktok.com" || hostname == "vt.tiktok.com" {
		return true
	}
	return hostMatches(hostname, "tiktok.com") &&
		strings.HasPrefix(strings.ToLower(parsed.Path), "/t/")
}

// Check if URL is a Facebook video URL (reels, watch, and page videos)
func IsFacebookURL(url string) bool {
	lowerURL := strings.ToLower(url)
	if strings.Contains(lowerURL, "fb.watch/") {
		return true
	}
	if !strings.Contains(lowerURL, "facebook.com") {
		return false
	}
	return strings.Contains(lowerURL, "/reel/") ||
		strings.Contains(lowerURL, "/videos/") ||
		strings.Contains(lowerURL, "/watch")
}

// Check if URL is a video URL (YouTube, Vimeo, Instagram, TikTok, Facebook, etc.)
//
// Instagram feed posts are excluded: they are photo carousels far more often
// than video, and routing them here is what produced a ~87% yt-dlp failure rate
// on /p/ URLs. They are handled by IsGalleryDLURL instead. TikTok photo posts
// are excluded for the same reason: yt-dlp cannot download a slideshow.
func IsVideoURL(url string) bool {
	if IsInstagramPhotoPostURL(url) || IsTikTokPhotoPostURL(url) {
		return false
	}
	return IsYouTubeURL(url) || IsVimeoURL(url) || IsInstagramURL(url) || IsTikTokURL(url) || IsFacebookURL(url)
}

// galleryDLSite matches a host against the URL path shape that identifies a
// single post/album on that host. gallery-dl also supports whole profiles,
// tags, and feeds, but Arker archives one page at a time, so only post-shaped
// URLs are routed here — a profile URL would otherwise pull down an entire
// account.
type galleryDLSite struct {
	// host is matched against the registrable hostname, exactly or as a parent
	// domain, so "x.com" covers www.x.com but never netflix.com.
	host string
	// paths are path prefixes/segments; an empty list means the host alone is
	// enough. Matched against the URL path only, never the query string.
	paths []string
	// requiresCookies marks sites that serve nothing at all to logged-out
	// clients, so a capture without a cookie jar is guaranteed to fail.
	requiresCookies bool
}

// galleryDLSites is the set of sites Arker sends to gallery-dl. gallery-dl
// itself supports ~300 sites; this list is deliberately narrower so that a
// gallery-dl item is only created when it has a real chance of succeeding.
// Adding a site is a one-line change here.
var galleryDLSites = []galleryDLSite{
	// Instagram feed posts. Reels stay on yt-dlp (see IsVideoURL).
	// Logged out, Instagram redirects every request to its login page.
	{host: "instagram.com", paths: []string{"/p/", "/tv/"}, requiresCookies: true},
	// TikTok photo posts (slideshows). Videos and short links stay on yt-dlp
	// (see IsVideoURL / IsTikTokShortLinkURL); gallery-dl's tiktok extractor
	// pulls each still straight from its CDN URL and works logged out.
	{host: "tiktok.com", paths: []string{"/photo/"}},
	{host: "twitter.com", paths: []string{"/status/"}, requiresCookies: true},
	{host: "x.com", paths: []string{"/status/"}, requiresCookies: true},
	{host: "pinterest.com", paths: []string{"/pin/"}, requiresCookies: true},
	{host: "reddit.com", paths: []string{"/comments/"}},
	{host: "redd.it"},
	{host: "tumblr.com", paths: []string{"/post/"}},
	{host: "bsky.app", paths: []string{"/post/"}},
	{host: "flickr.com", paths: []string{"/photos/"}},
	{host: "imgur.com", paths: []string{"/a/", "/gallery/", "/t/"}},
	{host: "deviantart.com", paths: []string{"/art/"}},
	{host: "artstation.com", paths: []string{"/artwork/"}},
	{host: "pixiv.net", paths: []string{"/artworks/"}},
	{host: "newgrounds.com", paths: []string{"/art/view/"}},
	{host: "vsco.co", paths: []string{"/media/"}},
}

// IsGalleryDLURL reports whether a URL points at a post whose media gallery-dl
// should download.
//
// Host and path are matched separately against the parsed URL rather than by
// substring over the whole string. Substring matching is what would let
// "x.com" match netflix.com, or a "/p/" anywhere in a query string route an
// unrelated site here.
func IsGalleryDLURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return false
	}
	path := strings.ToLower(parsed.Path)
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	for _, site := range galleryDLSites {
		if !hostMatches(hostname, site.host) {
			continue
		}
		if len(site.paths) == 0 {
			// Host-only entries (e.g. redd.it) still need a path to identify a
			// specific item; a bare domain is a homepage, not a post.
			if len(strings.Trim(path, "/")) > 0 {
				return true
			}
			continue
		}
		for _, sitePath := range site.paths {
			if strings.Contains(path, sitePath) {
				return true
			}
		}
	}
	return false
}

// hostMatches reports whether hostname is the given domain or a subdomain of
// it, so "www.instagram.com" matches "instagram.com" but "notinstagram.com"
// does not.
func hostMatches(hostname, domain string) bool {
	return hostname == domain || strings.HasSuffix(hostname, "."+domain)
}

// GalleryDLURLRequiresCookies reports whether a gallery-dl URL is on a site
// that serves nothing to logged-out clients.
func GalleryDLURLRequiresCookies(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	for _, site := range galleryDLSites {
		if site.requiresCookies && hostMatches(hostname, site.host) {
			return true
		}
	}
	return false
}

// ShouldCreateGalleryDLItem reports whether a capture of this URL should get a
// gallery-dl item at all.
//
// A login-only site with no cookie jar configured cannot succeed, so queueing
// it buys a guaranteed-failed archive item and spends real requests against the
// site getting there — which is how an unattended archiver walks itself into a
// rate limit that then affects the URLs it *could* have captured. Sites that
// work anonymously are unaffected.
func ShouldCreateGalleryDLItem(rawURL string) bool {
	if !IsGalleryDLURL(rawURL) {
		return false
	}
	if GalleryDLURLRequiresCookies(rawURL) && !MediaCookiesConfigured() {
		// Without cookies the native run is guaranteed to fail, but for
		// Instagram a configured Bright Data fallback gives the item a real
		// path to success, so it is worth creating (and paying for) after the
		// native attempt fails. Other login-only sites have no fallback and
		// stay excluded.
		if BrightDataMediaFallbackEnabled() && IsInstagramURL(rawURL) {
			return true
		}
		return false
	}
	return true
}

// IsSocialMediaPostURL reports whether a URL is a social-media post Arker
// claims to archive as a post, as opposed to an ordinary web page.
//
// This is the single source of truth for "recognized social post". Routing
// (GetArchiveTypes) and the unified archive API must agree on it: a URL that
// gets a media archive item but is not recognized would be served as a plain
// page capture, and a URL that is recognized but gets no media item must
// surface an explicit failure rather than a green MHTML/screenshot-only
// archive. Deriving both from this one function is what keeps those two
// answers from drifting apart.
//
// Recognized shapes, all of which route to yt-dlp or gallery-dl:
//
//	YouTube      /watch, /shorts/, youtu.be           -> yt-dlp
//	Vimeo        vimeo.com/<id>                       -> yt-dlp (via player URL)
//	Instagram    /reel/                               -> yt-dlp
//	Instagram    /p/, /tv/                            -> gallery-dl (cookies)
//	TikTok       /video/, vm/vt/t short links         -> yt-dlp
//	TikTok       /photo/                              -> gallery-dl
//	Facebook     /reel/, /videos/, /watch, fb.watch   -> yt-dlp
//	X/Twitter    /status/                             -> gallery-dl (cookies)
//	Reddit       /comments/, redd.it, v.redd.it       -> gallery-dl
//	Bluesky      /post/                               -> gallery-dl
//	Pinterest    /pin/                                -> gallery-dl (cookies)
//	Tumblr, Flickr, Imgur, DeviantArt, ArtStation,
//	Pixiv, Newgrounds, VSCO post shapes               -> gallery-dl
//
// Recognition is deliberately wider than item creation: a login-only site with
// no cookie jar gets no gallery-dl item (ShouldCreateGalleryDLItem), but the
// URL is still a recognized post, so the API answers with an explicit
// authentication_required failure instead of pretending nothing social was
// asked for.
//
// A TikTok short link that turns out to be a photo post is recognized here and
// routed to yt-dlp, which fails explicitly; resolving the redirect first would
// require a network call from this pure function. Facebook photo posts and
// other unclaimed shapes are NOT recognized and keep ordinary URL behavior.
func IsSocialMediaPostURL(rawURL string) bool {
	return IsVideoURL(rawURL) || IsGalleryDLURL(rawURL)
}

// Check if URL is an itch.io URL
func IsItchURL(url string) bool {
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, ".itch.io/")
}

// Get archive types based on URL patterns
func GetArchiveTypes(url string) []string {
	types := []string{ArchiveTypeMHTML, ArchiveTypeScreenshot}

	// Add itch archiver for itch.io URLs
	if IsItchURL(url) {
		types = append(types, ArchiveTypeItch)
	}

	// Add gallery-dl for photo posts and mixed photo/video carousels, unless
	// the site needs a login we do not have.
	if ShouldCreateGalleryDLItem(url) {
		types = append(types, ArchiveTypeGalleryDl)
	}

	// Add yt-dlp for video URLs (YouTube, Vimeo, Instagram reels, TikTok, etc.)
	if IsVideoURL(url) {
		types = append(types, ArchiveTypeYtDlp)
	}

	// Add Git archiver for Git repository URLs
	if IsGitURL(url) {
		types = append(types, ArchiveTypeGit)
	}

	return types
}

// Generate short ID
func GenerateShortID(db *gorm.DB) string {
	alphabet := []rune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	for {
		var sb strings.Builder
		for i := 0; i < 5; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			sb.WriteRune(alphabet[n.Int64()])
		}
		id := sb.String()
		var count int64
		db.Model(&models.Capture{}).Where("short_id = ?", id).Count(&count)
		if count == 0 {
			return id
		}
	}
}

// GenerateArchiveFilename creates a descriptive filename for archive downloads
func GenerateArchiveFilename(capture models.Capture, archivedURL models.ArchivedURL, extension string) string {
	// Format: YYYY-MM-DD_downcased_url.extension
	date := capture.CreatedAt.Format("2006-01-02")

	// Clean and downcase the URL
	url := strings.ToLower(archivedURL.Original)
	// Remove protocol
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	// Remove www
	url = strings.TrimPrefix(url, "www.")
	// Replace problematic characters with underscores
	url = strings.NewReplacer(
		"/", "_",
		"?", "_",
		"&", "_",
		"=", "_",
		"#", "_",
		":", "_",
		";", "_",
		" ", "_",
		"+", "_",
		"%", "_",
		".", "_",
	).Replace(url)
	// Remove trailing underscores and slashes
	url = strings.Trim(url, "_/")
	// Limit length to avoid filesystem issues
	if len(url) > 50 {
		url = url[:50]
	}

	// Remove leading dot from extension if present
	extension = strings.TrimPrefix(extension, ".")

	return fmt.Sprintf("%s_%s.%s", date, url, extension)
}

// StructurallySingleAssetGalleryURL reports whether a gallery-routed URL's
// shape guarantees exactly one media asset, so an archive holding one file is
// complete even when the extractor exposes no count field.
//
// Only shapes where the platform's own URL design rules out multi-asset posts
// belong here: a Flickr /photos/<user>/<id> page is one photo (albums live
// under /albums/), a Pinterest pin is one image, a VSCO /media/ page is one
// item, DeviantArt /art/ and Newgrounds /art/view/ are one artwork. Instagram,
// X, Reddit, Bluesky, Tumblr and Imgur posts can all carry several files and
// must never be listed.
func StructurallySingleAssetGalleryURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	path := strings.ToLower(parsed.Path)
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	switch {
	case hostMatches(hostname, "flickr.com"):
		// /photos/<user>/<id>/ is a single photo; /photos/<user>/albums/... and
		// /photos/<user>/sets/... are containers.
		if !strings.Contains(path, "/photos/") || strings.Contains(path, "/albums/") || strings.Contains(path, "/sets/") {
			return false
		}
		segments := strings.Split(strings.Trim(path, "/"), "/")
		return len(segments) >= 3
	case hostMatches(hostname, "pinterest.com"):
		return strings.Contains(path, "/pin/")
	case hostMatches(hostname, "vsco.co"):
		return strings.Contains(path, "/media/")
	case hostMatches(hostname, "deviantart.com"):
		return strings.Contains(path, "/art/")
	case hostMatches(hostname, "newgrounds.com"):
		return strings.Contains(path, "/art/view/")
	}
	return false
}
