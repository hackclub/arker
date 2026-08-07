package archivers

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"arker/internal/thumbnail"
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

// YtDlpArchiver downloads videos from YouTube, Vimeo, Instagram reels, TikTok
// and other platforms via yt-dlp. It handles video only; a URL whose media is
// photos (or a mixed photo/video carousel) belongs to GalleryDLArchiver, which
// yt-dlp rejects with "There is no video in this post".
type YtDlpArchiver struct{}

func (a *YtDlpArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (Result, error) {
	fmt.Fprintf(logWriter, "Starting video archive for: %s\n", url)

	// Check context before starting
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}

	// One private cookies copy for both yt-dlp runs in this job; yt-dlp
	// writes the jar back on exit, so it must not share a file with
	// concurrent jobs.
	cookieArgs, cleanupCookies, err := utils.YtDlpCookieArgsForRun()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to prepare yt-dlp cookies: %v\n", err)
		return Result{}, err
	}
	defer cleanupCookies()

	if version, versionErr := utils.YtDlpVersion(ctx); versionErr == nil {
		fmt.Fprintf(logWriter, "yt-dlp version: %s\n", version)
	} else {
		fmt.Fprintf(logWriter, "Could not determine yt-dlp version: %v\n", versionErr)
	}

	redactedLog := utils.NewRedactingWriter(logWriter, utils.YtDlpProxyRedactionSecrets())

	// Prepare command arguments
	testArgs := []string{"--print", "title,duration,uploader"}

	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.CommandContext(ctx, "yt-dlp")
	testCmd.Args = append(testCmd.Args, testArgs...)
	testCmd.Args = append(testCmd.Args, utils.YtDlpImpersonateArgsForURL(url)...)
	testCmd.Args = append(testCmd.Args, cookieArgs...)
	testCmd.Args = append(testCmd.Args, utils.YtDlpProxyArgs()...)
	testCmd.Args = append(testCmd.Args, url)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(redactedLog, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return Result{}, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(redactedLog, "Video info:\n%s\n", string(testOutput))

	// Check context before main download
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}

	tempBase, err := createTempVideoBase()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create temp video path: %v\n", err)
		return Result{}, err
	}
	keepTempFile := ""
	defer func() {
		cleanupTempVideoFilesExcept(tempBase, keepTempFile)
	}()

	outputTemplate := tempBase + ".%(ext)s"
	cmd := exec.CommandContext(ctx, "yt-dlp")
	cmd.Args = append(cmd.Args, ytDlpDownloadArgs(outputTemplate)...)
	cmd.Args = append(cmd.Args, utils.YtDlpImpersonateArgsForURL(url)...)
	cmd.Args = append(cmd.Args, cookieArgs...)
	cmd.Args = append(cmd.Args, utils.YtDlpProxyArgs()...)
	cmd.Args = append(cmd.Args, url)
	cmd.Stdout = redactedLog
	cmd.Stderr = redactedLog

	// Set process group so we can kill the entire process tree on timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	fmt.Fprintf(logWriter, "Starting yt-dlp download process...\n")
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start yt-dlp: %v\n", err)
		return Result{}, err
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
		return Result{}, fmt.Errorf("yt-dlp download failed: %w", err)
	}

	outputPath, err := findDownloadedMP4(tempBase)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to find downloaded MP4: %v\n", err)
		return Result{}, err
	}

	file, err := os.Open(outputPath)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to open downloaded MP4: %v\n", err)
		return Result{}, err
	}
	keepTempFile = outputPath

	// Read the poster image before returning: the deferred cleanup sweeps every
	// sibling temp file except the video, so it is gone the moment we return.
	thumb := videoThumbnail(tempBase, outputPath, logWriter)

	fmt.Fprintf(logWriter, "Video download completed successfully\n")

	return Result{
		Data:        &tempVideoReader{File: file, path: outputPath},
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Thumbnail:   thumb,
	}, nil
}

func ytDlpDownloadArgs(outputTemplate string) []string {
	return []string{
		"-f", "bestvideo+bestaudio/best",
		"--no-playlist",
		// Write the platform's own poster image next to the video. This is a
		// far better preview than a frame grab: it is the image the uploader or
		// the platform chose to represent the video, already framed on the
		// subject. It costs one CDN GET and yt-dlp treats a thumbnail failure
		// as a warning, so it cannot fail the download.
		"--write-thumbnail",
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"--verbose",
		"-o", outputTemplate,
	}
}

// thumbnailImageExtensions are the formats yt-dlp may write a poster image in.
// No --convert-thumbnail: that would shell out to ffmpeg for no gain, since the
// thumbnail package decodes all of these already.
var thumbnailImageExtensions = []string{".jpg", ".jpeg", ".webp", ".png"}

// videoThumbnail loads the poster image yt-dlp wrote beside the video.
//
// Returns nil for every failure. A missing or broken poster is not an archive
// problem: the video downloaded fine and the preview is cosmetic.
func videoThumbnail(tempBase, videoPath string, logWriter io.Writer) *Thumbnail {
	path := findDownloadedThumbnail(tempBase, videoPath)
	if path == "" {
		fmt.Fprintf(logWriter, "No thumbnail written by yt-dlp; skipping preview\n")
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(logWriter, "Could not open thumbnail %s: %v\n", filepath.Base(path), err)
		return nil
	}
	defer f.Close()

	// CropCenter, not CropTop: a vertical reel cover frames its subject in the
	// middle, so taking the top band would return the empty space above them.
	t, err := thumbnail.FromReader(f, thumbnail.CropCenter)
	if err != nil {
		fmt.Fprintf(logWriter, "Thumbnail generation skipped: %v\n", err)
		return nil
	}

	fmt.Fprintf(logWriter, "Thumbnail generated from %s: %dx%d, %d bytes\n",
		filepath.Base(path), t.Width, t.Height, len(t.Data))
	return &Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height}
}

// findDownloadedThumbnail locates the poster image among the temp files.
//
// It must not return the video itself, and remuxing can leave intermediate
// files sharing the same base, so this matches on image extension only.
func findDownloadedThumbnail(tempBase, videoPath string) string {
	matches, err := filepath.Glob(tempBase + "*")
	if err != nil {
		return ""
	}
	for _, match := range matches {
		if match == videoPath {
			continue
		}
		ext := strings.ToLower(filepath.Ext(match))
		for _, candidate := range thumbnailImageExtensions {
			if ext == candidate {
				return match
			}
		}
	}
	return ""
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
