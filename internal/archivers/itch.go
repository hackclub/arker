package archivers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"gorm.io/gorm"
)

// ItchArchiver downloads games from itch.io using itch-dl
type ItchArchiver struct {
	ItchDlPath string
	APIKey     string
}

// ItchMetadata represents the metadata structure we'll store
type ItchMetadata struct {
	GameID      int                    `json:"game_id"`
	Title       string                 `json:"title"`
	URL         string                 `json:"url"`
	CoverURL    string                 `json:"cover_url"`
	Screenshots []string               `json:"screenshots"`
	Description string                 `json:"description"`
	PublishedAt string                 `json:"published_at"`
	UpdatedAt   string                 `json:"updated_at,omitempty"`
	Author      string                 `json:"author"`
	AuthorURL   string                 `json:"author_url"`
	Extra       map[string]interface{} `json:"extra"`
	Rating      map[string]interface{} `json:"rating,omitempty"`
	Errors      []string               `json:"errors,omitempty"`
	Platforms   []string               `json:"platforms"`
	IsWebGame   bool                   `json:"is_web_game"`
	GameFiles   []GameFile             `json:"game_files"`
}

type GameFile struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Size     int64  `json:"size"`
}

func (a *ItchArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting itch archive for: %s\n", url)

	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}

	// Check if API key is available
	if a.APIKey == "" {
		return nil, "", "", nil, fmt.Errorf("itch.io API key not configured")
	}

	// Create temporary directory for itch-dl output
	tmpDir, err := os.MkdirTemp("", "itch-archive-*")
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	// Note: Don't defer cleanup here - it will happen before ZIP creation completes

	fmt.Fprintf(logWriter, "Created temp directory: %s\n", tmpDir)

	// Run itch-dl command
	fmt.Fprintf(logWriter, "Running itch-dl to download game...\n")
	cmd := exec.CommandContext(ctx, "python3", "-m", "itch_dl", "--api-key", a.APIKey, "--mirror-web", url)
	cmd.Dir = tmpDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture output
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(logWriter, "itch-dl error: %v\nOutput: %s\n", err, string(output))
		return nil, "", "", nil, fmt.Errorf("itch-dl failed: %w", err)
	}

	fmt.Fprintf(logWriter, "itch-dl completed successfully\n")
	fmt.Fprintf(logWriter, "itch-dl output: %s\n", string(output))

	// Find the downloaded game directory
	gameDir, err := findGameDirectory(tmpDir)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("failed to find game directory: %w", err)
	}

	fmt.Fprintf(logWriter, "Found game directory: %s\n", gameDir)

	// Parse metadata
	metadata, err := parseItchMetadata(gameDir, logWriter)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	fmt.Fprintf(logWriter, "Parsed metadata: %s\n", metadata.Title)

	// Create ZIP archive using io.Pipe for streaming
	pipeReader, pipeWriter := io.Pipe()

	go func() {
		defer pipeWriter.Close()
		defer os.RemoveAll(tmpDir) // Clean up temp directory after ZIP creation

		zipWriter := zip.NewWriter(pipeWriter)
		defer zipWriter.Close()

		// Add all files to ZIP with Store method (no compression)
		if err := addGameToZip(zipWriter, gameDir, metadata, logWriter); err != nil {
			fmt.Fprintf(logWriter, "Error adding files to ZIP: %v\n", err)
			pipeWriter.CloseWithError(err)
			return
		}

		fmt.Fprintf(logWriter, "Successfully created ZIP archive\n")
	}()

	return pipeReader, ".zip", "application/zip", nil, nil
}

// findGameDirectory locates the downloaded game directory
func findGameDirectory(tmpDir string) (string, error) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return "", err
	}

	// Look for subdirectories (itch-dl creates author/game structure)
	for _, entry := range entries {
		if entry.IsDir() {
			authorDir := filepath.Join(tmpDir, entry.Name())
			gameEntries, err := os.ReadDir(authorDir)
			if err != nil {
				continue
			}

			// Find the first game directory
			for _, gameEntry := range gameEntries {
				if gameEntry.IsDir() {
					return filepath.Join(authorDir, gameEntry.Name()), nil
				}
			}
		}
	}

	return "", fmt.Errorf("no game directory found in %s", tmpDir)
}

// parseItchMetadata reads and parses the metadata.json file
func parseItchMetadata(gameDir string, logWriter io.Writer) (*ItchMetadata, error) {
	metadataPath := filepath.Join(gameDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata.json: %w", err)
	}

	var metadata ItchMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata.json: %w", err)
	}

	// Detect if it's a web game by looking for HTML5 platform or index.html
	metadata.IsWebGame = detectWebGame(gameDir, &metadata)

	// Extract platform information and file details
	metadata.Platforms = extractPlatforms(&metadata)
	metadata.GameFiles = extractGameFiles(gameDir, &metadata)

	fmt.Fprintf(logWriter, "Game type: web=%t, platforms=%v\n", metadata.IsWebGame, metadata.Platforms)

	return &metadata, nil
}

// detectWebGame checks if this is a playable web game
func detectWebGame(gameDir string, metadata *ItchMetadata) bool {
	// Check if platforms include HTML5
	if platforms, ok := metadata.Extra["platforms"].([]interface{}); ok {
		for _, platform := range platforms {
			if platformStr, ok := platform.(string); ok {
				if strings.ToLower(platformStr) == "html5" {
					return true
				}
			}
		}
	}

	// Check for web game ZIP files in files directory
	filesDir := filepath.Join(gameDir, "files")
	if entries, err := os.ReadDir(filesDir); err == nil {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".zip") {
				// Assume ZIP files in HTML5 games are web games
				return true
			}
		}
	}

	return false
}

// extractPlatforms gets the list of platforms from metadata
func extractPlatforms(metadata *ItchMetadata) []string {
	var platforms []string

	if platformsInterface, ok := metadata.Extra["platforms"].([]interface{}); ok {
		for _, platform := range platformsInterface {
			if platformStr, ok := platform.(string); ok {
				platforms = append(platforms, platformStr)
			}
		}
	}

	return platforms
}

// extractGameFiles analyzes the files directory to get game file information
func extractGameFiles(gameDir string, metadata *ItchMetadata) []GameFile {
	var gameFiles []GameFile

	filesDir := filepath.Join(gameDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return gameFiles
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				continue
			}

			// Try to detect platform from filename
			platform := detectPlatformFromFilename(entry.Name())

			gameFiles = append(gameFiles, GameFile{
				Name:     entry.Name(),
				Platform: platform,
				Size:     info.Size(),
			})
		}
	}

	return gameFiles
}

// detectPlatformFromFilename tries to guess platform from filename
func detectPlatformFromFilename(filename string) string {
	lower := strings.ToLower(filename)

	if strings.Contains(lower, "win") || strings.Contains(lower, "windows") {
		return "Windows"
	}
	if strings.Contains(lower, "mac") || strings.Contains(lower, "osx") || strings.Contains(lower, "darwin") {
		return "macOS"
	}
	if strings.Contains(lower, "linux") || strings.Contains(lower, "unix") {
		return "Linux"
	}
	if strings.Contains(lower, ".exe") {
		return "Windows"
	}

	return "Unknown"
}

// addGameToZip adds all game files to the ZIP archive exactly as provided by itch-dl
func addGameToZip(zipWriter *zip.Writer, gameDir string, metadata *ItchMetadata, logWriter io.Writer) error {
	// Add all files from itch-dl output exactly as they are
	return addDirectoryToZip(zipWriter, gameDir, "", logWriter)
}

// addDirectoryToZip recursively adds all files from a directory to the ZIP archive
func addDirectoryToZip(zipWriter *zip.Writer, sourceDir, zipPrefix string, logWriter io.Writer) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		sourcePath := filepath.Join(sourceDir, entry.Name())
		zipPath := entry.Name()
		if zipPrefix != "" {
			zipPath = filepath.Join(zipPrefix, entry.Name())
		}

		if entry.IsDir() {
			// Recursively add subdirectory
			if err := addDirectoryToZip(zipWriter, sourcePath, zipPath, logWriter); err != nil {
				fmt.Fprintf(logWriter, "Warning: failed to add directory %s: %v\n", zipPath, err)
			}
		} else {
			// Add file as-is
			if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
				fmt.Fprintf(logWriter, "Warning: failed to add file %s: %v\n", zipPath, err)
			}
		}
	}

	return nil
}

// addFileFromDiskToZip adds a file from disk to the ZIP archive
func addFileFromDiskToZip(zipWriter *zip.Writer, zipPath, sourcePath string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header := &zip.FileHeader{
		Name:   zipPath,
		Method: zip.Deflate, // Use ZIP compression
	}
	header.SetModTime(info.ModTime())

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, file)
	return err
}
