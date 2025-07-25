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
	return strings.HasSuffix(lowerURL, ".git") ||
		strings.Contains(lowerURL, "github.com/") ||
		strings.Contains(lowerURL, "gitlab.com/") ||
		strings.Contains(lowerURL, "bitbucket.org/") ||
		strings.Contains(lowerURL, "codeberg.org/") ||
		strings.Contains(lowerURL, "git.")
}

// Check if URL is a YouTube URL
func IsYouTubeURL(url string) bool {
	lowerURL := strings.ToLower(url)
	return strings.Contains(lowerURL, "youtube.com") || strings.Contains(lowerURL, "youtu.be")
}

// Get archive types based on URL patterns
func GetArchiveTypes(url string) []string {
	types := []string{"mhtml", "screenshot"}
	lowerURL := strings.ToLower(url)
	
	// Add YouTube archiver for YouTube URLs
	if strings.Contains(lowerURL, "youtube.com") || strings.Contains(lowerURL, "youtu.be") {
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
		for i := 0; i < 8; i++ {
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
