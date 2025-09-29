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

	// Check for index.html in files directory
	filesDir := filepath.Join(gameDir, "files")
	if entries, err := os.ReadDir(filesDir); err == nil {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".zip") {
				// Check inside ZIP files for index.html
				if hasIndexHTML(filepath.Join(filesDir, entry.Name())) {
					return true
				}
			}
		}
	}

	return false
}

// hasIndexHTML checks if a ZIP file contains index.html
func hasIndexHTML(zipPath string) bool {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return false
	}
	defer reader.Close()

	for _, file := range reader.File {
		if strings.HasSuffix(strings.ToLower(file.Name), "index.html") {
			return true
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

// addGameToZip adds all game files to the ZIP archive
func addGameToZip(zipWriter *zip.Writer, gameDir string, metadata *ItchMetadata, logWriter io.Writer) error {
	// Add metadata.json
	if err := addFileToZip(zipWriter, "metadata.json", metadata, true); err != nil {
		return fmt.Errorf("failed to add metadata.json: %w", err)
	}

	// Add site.html
	siteHTML := generateSiteHTML(metadata)
	if err := addFileToZip(zipWriter, "site.html", []byte(siteHTML), false); err != nil {
		return fmt.Errorf("failed to add site.html: %w", err)
	}

	// Add cover image if exists
	coverPath := filepath.Join(gameDir, "cover.png")
	if _, err := os.Stat(coverPath); err == nil {
		if err := addFileFromDiskToZip(zipWriter, "cover.png", coverPath); err != nil {
			fmt.Fprintf(logWriter, "Warning: failed to add cover.png: %v\n", err)
		}
	}

	// Add screenshots
	screenshotsDir := filepath.Join(gameDir, "screenshots")
	if entries, err := os.ReadDir(screenshotsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				sourcePath := filepath.Join(screenshotsDir, entry.Name())
				zipPath := filepath.Join("screenshots", entry.Name())
				if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
					fmt.Fprintf(logWriter, "Warning: failed to add screenshot %s: %v\n", entry.Name(), err)
				}
			}
		}
	}

	// Add game files
	if err := addGameFiles(zipWriter, gameDir, metadata, logWriter); err != nil {
		return fmt.Errorf("failed to add game files: %w", err)
	}

	return nil
}

// addGameFiles adds the actual game files to the ZIP
func addGameFiles(zipWriter *zip.Writer, gameDir string, metadata *ItchMetadata, logWriter io.Writer) error {
	// Debug: List what's actually in the game directory
	fmt.Fprintf(logWriter, "Contents of game directory %s:\n", gameDir)
	gameEntries, err := os.ReadDir(gameDir)
	if err != nil {
		return fmt.Errorf("failed to read game directory: %w", err)
	}
	
	for _, entry := range gameEntries {
		fmt.Fprintf(logWriter, "  %s (dir: %t)\n", entry.Name(), entry.IsDir())
	}
	
	filesDir := filepath.Join(gameDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		fmt.Fprintf(logWriter, "Files directory doesn't exist at %s, checking for direct files in game directory\n", filesDir)
		
		// If files directory doesn't exist, look for game files directly in gameDir
		for _, entry := range gameEntries {
			if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".zip") || 
								  strings.HasSuffix(entry.Name(), ".exe") ||
								  strings.HasSuffix(entry.Name(), ".tar.gz") ||
								  strings.HasSuffix(entry.Name(), ".tar.bz2")) {
				sourcePath := filepath.Join(gameDir, entry.Name())
				zipPath := filepath.Join("game", entry.Name())
				fmt.Fprintf(logWriter, "Adding game file directly from game dir: %s\n", entry.Name())
				if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		sourcePath := filepath.Join(filesDir, entry.Name())
		
		// If it's a web game and this is a ZIP file containing index.html, extract it
		if metadata.IsWebGame && strings.HasSuffix(entry.Name(), ".zip") && hasIndexHTML(sourcePath) {
			fmt.Fprintf(logWriter, "Extracting web game from %s\n", entry.Name())
			if err := extractWebGameToZip(zipWriter, sourcePath, logWriter); err != nil {
				fmt.Fprintf(logWriter, "Failed to extract web game: %v\n", err)
				// Fall back to adding the ZIP file as-is
				zipPath := filepath.Join("game", entry.Name())
				if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
					return err
				}
			}
		} else {
			// Add file as-is to game directory
			zipPath := filepath.Join("game", entry.Name())
			if err := addFileFromDiskToZip(zipWriter, zipPath, sourcePath); err != nil {
				return err
			}
		}
	}

	return nil
}

// extractWebGameToZip extracts a web game ZIP into the archive's game/ directory
func extractWebGameToZip(zipWriter *zip.Writer, webGameZipPath string, logWriter io.Writer) error {
	reader, err := zip.OpenReader(webGameZipPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.File {
		// Skip directories
		if file.FileInfo().IsDir() {
			continue
		}

		// Create path in game/ directory
		zipPath := filepath.Join("game", file.Name)
		
		// Open source file
		srcFile, err := file.Open()
		if err != nil {
			fmt.Fprintf(logWriter, "Warning: failed to open %s: %v\n", file.Name, err)
			continue
		}

		// Create ZIP entry with Deflate compression
		header := &zip.FileHeader{
			Name:   zipPath,
			Method: zip.Deflate, // Use ZIP compression
		}
		header.SetModTime(file.FileHeader.Modified)

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			srcFile.Close()
			return err
		}

		// Copy file content
		_, err = io.Copy(writer, srcFile)
		srcFile.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// addFileToZip adds data to the ZIP archive
func addFileToZip(zipWriter *zip.Writer, name string, data interface{}, isJSON bool) error {
	var content []byte
	var err error

	if isJSON {
		content, err = json.MarshalIndent(data, "", "  ")
		if err != nil {
			return err
		}
	} else {
		content = data.([]byte)
	}

	header := &zip.FileHeader{
		Name:   name,
		Method: zip.Deflate, // Use ZIP compression
	}

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = writer.Write(content)
	return err
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

// generateSiteHTML creates the site.html content
func generateSiteHTML(metadata *ItchMetadata) string {
	if metadata.IsWebGame {
		// For web games, create an HTML that loads the game
		return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
        body { margin: 0; padding: 20px; font-family: Arial, sans-serif; }
        .game-container { width: 100%%; height: 600px; border: 1px solid #ccc; }
        .game-frame { width: 100%%; height: 100%%; border: none; }
        .info { margin-bottom: 20px; }
        h1 { margin: 0 0 10px 0; }
        .author { color: #666; margin-bottom: 10px; }
        .description { margin-bottom: 20px; }
    </style>
</head>
<body>
    <div class="info">
        <h1>%s</h1>
        <div class="author">by %s</div>
        <div class="description">%s</div>
    </div>
    <div class="game-container">
        <iframe class="game-frame" src="game/temp/index.html" allowfullscreen></iframe>
    </div>
</body>
</html>`, metadata.Title, metadata.Title, metadata.Author, metadata.Description)
	} else {
		// For desktop games, create a simple info page
		return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
        body { margin: 0; padding: 20px; font-family: Arial, sans-serif; max-width: 800px; }
        h1 { margin: 0 0 10px 0; }
        .author { color: #666; margin-bottom: 10px; }
        .description { margin: 20px 0; line-height: 1.5; }
        .downloads { margin-top: 20px; }
        .download-item { margin: 10px 0; }
        .download-link { display: inline-block; padding: 10px 15px; background: #0066cc; color: white; text-decoration: none; border-radius: 5px; }
        .download-link:hover { background: #0052a3; }
    </style>
</head>
<body>
    <h1>%s</h1>
    <div class="author">by %s</div>
    <div class="description">%s</div>
    <div class="downloads">
        <h3>Downloads:</h3>
        <p>This game is not playable in the browser. Download files are available in the full archive.</p>
    </div>
</body>
</html>`, metadata.Title, metadata.Title, metadata.Author, metadata.Description)
	}
}
