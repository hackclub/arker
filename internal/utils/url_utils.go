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

// Check if URL is a TikTok URL (full video links and vm/vt/t short links)
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
// on /p/ URLs. They are handled by IsGalleryDLURL instead.
func IsVideoURL(url string) bool {
	if IsInstagramPhotoPostURL(url) {
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
}

// galleryDLSites is the set of sites Arker sends to gallery-dl. gallery-dl
// itself supports ~300 sites; this list is deliberately narrower so that a
// gallery-dl item is only created when it has a real chance of succeeding.
// Adding a site is a one-line change here.
var galleryDLSites = []galleryDLSite{
	// Instagram feed posts. Reels stay on yt-dlp (see IsVideoURL).
	{host: "instagram.com", paths: []string{"/p/", "/tv/"}},
	{host: "twitter.com", paths: []string{"/status/"}},
	{host: "x.com", paths: []string{"/status/"}},
	{host: "reddit.com", paths: []string{"/comments/"}},
	{host: "redd.it"},
	{host: "tumblr.com", paths: []string{"/post/"}},
	{host: "bsky.app", paths: []string{"/post/"}},
	{host: "flickr.com", paths: []string{"/photos/"}},
	{host: "imgur.com", paths: []string{"/a/", "/gallery/", "/t/"}},
	{host: "deviantart.com", paths: []string{"/art/"}},
	{host: "artstation.com", paths: []string{"/artwork/"}},
	{host: "pixiv.net", paths: []string{"/artworks/"}},
	{host: "pinterest.com", paths: []string{"/pin/"}},
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

	// Add gallery-dl for photo posts and mixed photo/video carousels
	if IsGalleryDLURL(url) {
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
