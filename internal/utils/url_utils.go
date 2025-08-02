package utils

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"gorm.io/gorm"
	"arker/internal/models"
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

// Check if URL is a video URL (YouTube, Vimeo, etc.)
func IsVideoURL(url string) bool {
	return IsYouTubeURL(url) || IsVimeoURL(url)
}

// Get archive types based on URL patterns
func GetArchiveTypes(url string) []string {
	types := []string{"mhtml", "screenshot"}
	
	// Add YouTube archiver for video URLs (YouTube, Vimeo, etc.)
	if IsVideoURL(url) {
		types = append(types, "youtube")
	}
	
	// Add Git archiver for Git repository URLs
	if IsGitURL(url) {
		types = append(types, "git")
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
