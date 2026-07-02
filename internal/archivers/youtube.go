package archivers

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"arker/internal/utils"
)

type tempVideoReader struct {
	*os.File
	path string
}

func (r *tempVideoReader) Close() error {
	err1 := r.File.Close()
	err2 := os.Remove(r.path)
	if err1 != nil {
		return err1
	}
	return err2
}

// YTArchiver downloads videos from YouTube, Vimeo, and other platforms (streams directly from yt-dlp stdout)
type YTArchiver struct{}

func (a *YTArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting video archive for: %s\n", url)

	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}

	// One private cookies copy for both yt-dlp runs in this job; yt-dlp
	// writes the jar back on exit, so it must not share a file with
	// concurrent jobs.
	cookieArgs, cleanupCookies, err := utils.YtDlpCookieArgsForRun()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to prepare yt-dlp cookies: %v\n", err)
		return nil, "", "", nil, err
	}
	defer cleanupCookies()

	// Prepare command arguments
	testArgs := []string{"--print", "title,duration,uploader"}

	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.CommandContext(ctx, "yt-dlp")
	testCmd.Args = append(testCmd.Args, testArgs...)
	testCmd.Args = append(testCmd.Args, cookieArgs...)
	testCmd.Args = append(testCmd.Args, url)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(logWriter, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return nil, "", "", nil, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(logWriter, "Video info:\n%s\n", string(testOutput))

	// Check context before main download
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}

	tempBase, err := createTempVideoBase()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create temp video path: %v\n", err)
		return nil, "", "", nil, err
	}
	keepTempFile := ""
	defer func() {
		cleanupTempVideoFilesExcept(tempBase, keepTempFile)
	}()

	outputTemplate := tempBase + ".%(ext)s"
	cmd := exec.CommandContext(ctx, "yt-dlp")
	cmd.Args = append(cmd.Args, ytDlpDownloadArgs(outputTemplate)...)
	cmd.Args = append(cmd.Args, cookieArgs...)
	cmd.Args = append(cmd.Args, url)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	// Set process group so we can kill the entire process tree on timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	fmt.Fprintf(logWriter, "Starting yt-dlp download process...\n")
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start yt-dlp: %v\n", err)
		return nil, "", "", nil, err
	}

	// Kill the whole process group when the context times out
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				fmt.Fprintf(logWriter, "Context cancelled, killing yt-dlp process group\n")
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	if err = cmd.Wait(); err != nil {
		fmt.Fprintf(logWriter, "yt-dlp download failed: %v\n", err)
		return nil, "", "", nil, fmt.Errorf("yt-dlp download failed: %w", err)
	}

	outputPath, err := findDownloadedMP4(tempBase)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to find downloaded MP4: %v\n", err)
		return nil, "", "", nil, err
	}

	file, err := os.Open(outputPath)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to open downloaded MP4: %v\n", err)
		return nil, "", "", nil, err
	}
	keepTempFile = outputPath

	fmt.Fprintf(logWriter, "Video download completed successfully\n")

	return &tempVideoReader{File: file, path: outputPath}, ".mp4", "video/mp4", nil, nil
}

func ytDlpDownloadArgs(outputTemplate string) []string {
	return []string{
		"-f", "bestvideo+bestaudio/best",
		"--no-playlist",
		"--no-write-thumbnail",
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"--verbose",
		"-o", outputTemplate,
	}
}

func createTempVideoBase() (string, error) {
	f, err := os.CreateTemp("", "arker-video-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func findDownloadedMP4(tempBase string) (string, error) {
	path := tempBase + ".mp4"
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	matches, err := filepath.Glob(tempBase + "*.mp4")
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no MP4 output found for %s", tempBase)
	}
	return matches[0], nil
}

func cleanupTempVideoFiles(tempBase string) {
	cleanupTempVideoFilesExcept(tempBase, "")
}

func cleanupTempVideoFilesExcept(tempBase, keep string) {
	matches, err := filepath.Glob(tempBase + "*")
	if err != nil {
		return
	}
	for _, match := range matches {
		if match == keep {
			continue
		}
		_ = os.Remove(match)
	}
}
