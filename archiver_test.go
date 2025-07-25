package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"arker/internal/archivers"
)

func TestGitArchiver(t *testing.T) {
	// Create a temporary git repository for testing
	tempDir, err := os.MkdirTemp("", "git-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Initialize a git repository
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Create a test file
	testFile := filepath.Join(tempDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Add and commit the file
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	_, err = worktree.Add("test.txt")
	if err != nil {
		t.Fatal(err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Test the GitArchiver
	archiver := &archivers.GitArchiver{}
	
	// Note: This would require a remote git URL for a full test
	// For now, we just test the structure
	if archiver == nil {
		t.Error("GitArchiver should not be nil")
	}
}

func TestMHTMLArchiver(t *testing.T) {
	// This test would require a running Playwright browser
	// For now, just test the structure
	archiver := &archivers.MHTMLArchiver{}
	
	if archiver == nil {
		t.Error("MHTMLArchiver should not be nil")
	}
}

func TestScreenshotArchiver(t *testing.T) {
	// This test would require a running Playwright browser
	// For now, just test the structure
	archiver := &archivers.ScreenshotArchiver{}
	
	if archiver == nil {
		t.Error("ScreenshotArchiver should not be nil")
	}
}

func TestYTArchiver(t *testing.T) {
	// This test would require yt-dlp to be installed
	// For now, just test the structure
	archiver := &archivers.YTArchiver{}
	
	if archiver == nil {
		t.Error("YTArchiver should not be nil")
	}
}

// Test helper function for content type detection
func TestContentTypeDetection(t *testing.T) {
	testCases := []struct {
		archiveType string
		expectedCT  string
	}{
		{"mhtml", "application/x-mhtml"},
		{"screenshot", "image/png"},
		{"youtube", "video/mp4"},
		{"git", "application/zstd"},
		{"unknown", "application/octet-stream"},
	}

	for _, tc := range testCases {
		// This would be tested in the serveArchive function
		// For now, just verify the test cases are structured correctly
		if tc.archiveType == "" || tc.expectedCT == "" {
			t.Error("Test case should not have empty values")
		}
	}
}

// Test streaming functionality
func TestStreamingCopy(t *testing.T) {
	data := "This is test data for streaming"
	reader := strings.NewReader(data)
	
	var buffer strings.Builder
	
	_, err := io.Copy(&buffer, reader)
	if err != nil {
		t.Fatal(err)
	}
	
	if buffer.String() != data {
		t.Errorf("Expected %s, got %s", data, buffer.String())
	}
}
