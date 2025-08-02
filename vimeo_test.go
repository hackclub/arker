package main

import (
	"testing"

	"arker/internal/utils"
)

// Test that demonstrates the complete Vimeo support functionality
func TestVimeoEndToEnd(t *testing.T) {
	testURL := "https://vimeo.com/123456789"
	
	// 1. Verify Vimeo URL is detected as video URL
	if !utils.IsVimeoURL(testURL) {
		t.Fatal("Vimeo URL should be detected by IsVimeoURL")
	}
	
	if !utils.IsVideoURL(testURL) {
		t.Fatal("Vimeo URL should be detected by IsVideoURL")
	}
	
	// 2. Verify correct archive types are selected
	types := utils.GetArchiveTypes(testURL)
	expectedTypes := map[string]bool{
		"mhtml":     true,
		"screenshot": true,
		"youtube":   true, // The yt-dlp archiver handles Vimeo
	}
	
	if len(types) != len(expectedTypes) {
		t.Fatalf("Expected %d archive types, got %d: %v", len(expectedTypes), len(types), types)
	}
	
	for _, archiveType := range types {
		if !expectedTypes[archiveType] {
			t.Errorf("Unexpected archive type '%s' for Vimeo URL", archiveType)
		}
	}
	
	t.Logf("✅ Vimeo URL '%s' correctly gets archive types: %v", testURL, types)
}

func TestVimeoSupport(t *testing.T) {
	// Test Vimeo URL detection
	vimeoURLs := []string{
		"https://vimeo.com/123456789",
		"https://www.vimeo.com/video/123456789",
		"http://vimeo.com/123456789",
		"https://player.vimeo.com/video/123456789",
	}

	for _, url := range vimeoURLs {
		if !utils.IsVimeoURL(url) {
			t.Errorf("Expected %s to be detected as Vimeo URL", url)
		}
		
		if !utils.IsVideoURL(url) {
			t.Errorf("Expected %s to be detected as video URL", url)
		}
		
		// Check that Vimeo URLs get the youtube archiver type
		types := utils.GetArchiveTypes(url)
		hasYoutubeType := false
		for _, archiveType := range types {
			if archiveType == "youtube" {
				hasYoutubeType = true
				break
			}
		}
		if !hasYoutubeType {
			t.Errorf("Expected Vimeo URL %s to include 'youtube' archiver type, got: %v", url, types)
		}
	}

	// Test non-Vimeo URLs
	nonVimeoURLs := []string{
		"https://youtube.com/watch?v=123",
		"https://example.com",
		"https://github.com/user/repo",
		"https://vimeounrelated.com",
	}

	for _, url := range nonVimeoURLs {
		if utils.IsVimeoURL(url) {
			t.Errorf("Expected %s NOT to be detected as Vimeo URL", url)
		}
	}
}

func TestVideoURLDetection(t *testing.T) {
	// Test that both YouTube and Vimeo are detected as video URLs
	testCases := []struct {
		url      string
		isVideo  bool
		platform string
	}{
		{"https://youtube.com/watch?v=123", true, "YouTube"},
		{"https://youtu.be/123", true, "YouTube"},
		{"https://vimeo.com/123456789", true, "Vimeo"},
		{"https://www.vimeo.com/video/123", true, "Vimeo"},
		{"https://example.com", false, "Regular website"},
		{"https://github.com/user/repo", false, "Git repository"},
	}

	for _, tc := range testCases {
		result := utils.IsVideoURL(tc.url)
		if result != tc.isVideo {
			t.Errorf("IsVideoURL(%s) = %v, expected %v (%s)", tc.url, result, tc.isVideo, tc.platform)
		}
	}
}

func TestArchiveTypesWithVimeo(t *testing.T) {
	// Test that Vimeo URLs get the correct archive types
	vimeoURL := "https://vimeo.com/123456789"
	types := utils.GetArchiveTypes(vimeoURL)
	
	expectedTypes := []string{"mhtml", "screenshot", "youtube"}
	
	if len(types) != len(expectedTypes) {
		t.Errorf("Expected %d archive types for Vimeo URL, got %d: %v", len(expectedTypes), len(types), types)
	}
	
	for _, expected := range expectedTypes {
		found := false
		for _, actual := range types {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected archive type '%s' for Vimeo URL, but it was not found in: %v", expected, types)
		}
	}
}
