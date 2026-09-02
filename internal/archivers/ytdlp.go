package archivers

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"arker/internal/models"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

type tempVideoReader struct {
	*os.File
	path string
	dir  string
}

func (r *tempVideoReader) Close() error {
	err1 := r.File.Close()
	err2 := os.Remove(r.path)
	if r.dir != "" {
		_ = os.Remove(r.dir)
	}
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
	return a.archive(ctx, url, logWriter, true, VideoMedia{})
}

// RefreshSocialThumbnail asks yt-dlp for only the platform-authored poster.
// It deliberately avoids the normal metadata refresh path: that path also
// probes twice, fetches captions and writes sidecars, which is useful for a new
// capture but wasteful and unnecessarily noisy for thousands of historical
// thumbnail repairs.
func (a *YtDlpArchiver) RefreshSocialThumbnail(ctx context.Context, url string, logWriter io.Writer) (*Thumbnail, error) {
	if logWriter == nil {
		logWriter = io.Discard
	}
	cookieArgs, cleanupCookies, err := utils.YtDlpCookieArgsForRun()
	if err != nil {
		return nil, fmt.Errorf("prepare media cookies: %w", err)
	}
	defer cleanupCookies()

	tempBase, err := createTempVideoBase()
	if err != nil {
		return nil, fmt.Errorf("create thumbnail temp path: %w", err)
	}
	defer cleanupTempVideoFiles(tempBase)

	fetchURL := utils.YtDlpFetchURL(url)
	args := []string{
		"--skip-download",
		"--no-playlist",
		"--write-thumbnail",
		"--verbose",
		"-o", tempBase + ".%(ext)s",
	}
	args = append(args, utils.YtDlpImpersonateArgsForURL(url)...)
	args = append(args, utils.YtDlpRefererArgsForURL(fetchURL)...)
	args = append(args, cookieArgs...)
	args = append(args, utils.YtDlpProxyArgs()...)
	args = append(args, fetchURL)

	redactedLog := utils.NewRedactingWriter(logWriter, utils.YtDlpProxyRedactionSecrets())
	fmt.Fprintf(logWriter, "Refreshing provider poster with yt-dlp (media download disabled)\n")
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stdout = redactedLog
	cmd.Stderr = redactedLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp poster refresh failed: %w", err)
	}

	thumb := videoThumbnail(tempBase, "", logWriter)
	if thumb == nil {
		return nil, fmt.Errorf("%w: yt-dlp returned no supported poster image", ErrSocialThumbnailUnavailable)
	}
	return thumb, nil
}

// RefreshVideoMetadata re-runs the extractor with --skip-download to rebuild
// the metadata sidecars, caption tracks, and poster image for a video whose
// bytes are already archived. media describes the stored artifact, so the
// refreshed record keeps reporting the file the archive actually holds rather
// than whatever the extractor would pick today. The returned Result carries no
// Data.
func (a *YtDlpArchiver) RefreshVideoMetadata(ctx context.Context, url string, logWriter io.Writer, media VideoMedia) (Result, error) {
	return a.archive(ctx, url, logWriter, false, media)
}

// archive is the shared flow behind Archive and RefreshVideoMetadata. The two
// runs are identical — cookies, probe, subtitle selection, info JSON, poster —
// except that a metadata-only run passes --skip-download and describes the
// already-stored media instead of a freshly downloaded file.
func (a *YtDlpArchiver) archive(ctx context.Context, url string, logWriter io.Writer, downloadVideo bool, storedMedia VideoMedia) (Result, error) {
	if downloadVideo {
		fmt.Fprintf(logWriter, "Starting video archive for: %s\n", url)
	} else {
		fmt.Fprintf(logWriter, "Refreshing video metadata for: %s (media already archived)\n", url)
	}

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

	version := ""
	if detectedVersion, versionErr := utils.YtDlpVersion(ctx); versionErr == nil {
		version = detectedVersion
		fmt.Fprintf(logWriter, "yt-dlp version: %s\n", version)
	} else {
		fmt.Fprintf(logWriter, "Could not determine yt-dlp version: %v\n", versionErr)
	}

	redactedLog := utils.NewRedactingWriter(logWriter, utils.YtDlpProxyRedactionSecrets())

	// Some platforms serve the video from somewhere other than the page a
	// human visits (Vimeo's main site is login-only; its player is not). Only
	// the fetch target changes — the archive stays recorded against url.
	fetchURL := utils.YtDlpFetchURL(url)
	if fetchURL != url {
		fmt.Fprintf(logWriter, "Fetching via %s (the page URL is not retrievable without an account)\n", fetchURL)
	}
	refererArgs := utils.YtDlpRefererArgsForURL(fetchURL)
	refererArgs = append(refererArgs, utils.YtDlpFormatFallbackArgsForURL(fetchURL)...)

	// Prepare command arguments. The language is printed last and on its own so
	// the subtitle filter can be built from the video's actual language rather
	// than guessed; yt-dlp prints "NA" when it does not know.
	testArgs := []string{"--print", "title,duration,uploader", "--print", "%(language)s"}

	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.CommandContext(ctx, "yt-dlp")
	testCmd.Args = append(testCmd.Args, testArgs...)
	testCmd.Args = append(testCmd.Args, utils.YtDlpImpersonateArgsForURL(url)...)
	testCmd.Args = append(testCmd.Args, refererArgs...)
	testCmd.Args = append(testCmd.Args, cookieArgs...)
	testCmd.Args = append(testCmd.Args, utils.YtDlpProxyArgs()...)
	testCmd.Args = append(testCmd.Args, fetchURL)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(redactedLog, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return Result{}, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(redactedLog, "Video info:\n%s\n", string(testOutput))
	detectedLang := detectedLanguageFromProbe(string(testOutput))
	if detectedLang != "" {
		fmt.Fprintf(logWriter, "Detected video language: %s\n", detectedLang)
	}

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
	if !downloadVideo {
		// The media bytes are already archived; only the info JSON, captions,
		// and poster image are wanted from this run.
		cmd.Args = append(cmd.Args, "--skip-download")
	}
	cmd.Args = append(cmd.Args, utils.YtDlpSubtitleArgs(detectedLang)...)
	cmd.Args = append(cmd.Args, utils.YtDlpImpersonateArgsForURL(url)...)
	cmd.Args = append(cmd.Args, refererArgs...)
	cmd.Args = append(cmd.Args, cookieArgs...)
	cmd.Args = append(cmd.Args, utils.YtDlpProxyArgs()...)
	cmd.Args = append(cmd.Args, fetchURL)
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

	outputPath := ""
	media := storedMedia
	if downloadVideo {
		outputPath, err = findDownloadedMP4(tempBase)
		if err != nil {
			fmt.Fprintf(logWriter, "Failed to find downloaded MP4: %v\n", err)
			return Result{}, err
		}
		videoStat, err := os.Stat(outputPath)
		if err != nil {
			fmt.Fprintf(logWriter, "Failed to inspect downloaded MP4: %v\n", err)
			return Result{}, err
		}
		media = VideoMedia{
			Extension:   ".mp4",
			ContentType: "video/mp4",
			SizeBytes:   videoStat.Size(),
		}
	}

	infoPath, err := findYtDlpInfoJSON(tempBase)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to find yt-dlp info JSON: %v\n", err)
		return Result{}, err
	}
	rawInfo, err := os.ReadFile(infoPath)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to read yt-dlp info JSON: %v\n", err)
		return Result{}, err
	}
	metadata, sanitizedRaw, err := BuildYtDlpVideoArtifacts(rawInfo, url, version, media, time.Now())
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to normalize yt-dlp info JSON: %v\n", err)
		return Result{}, err
	}
	// Captions are read here, before the deferred cleanup sweeps the temp
	// directory. A platform that exposes none is the normal case and must not
	// disturb the capture, so every failure below is logged and dropped.
	extras, tracks, contents := collectSubtitleArtifacts(tempBase, rawInfo, logWriter)
	metadata.Subtitles = tracks
	metadata.Transcript = BuildTranscript(tracks, contents, detectedLang)
	logSubtitleOutcome(logWriter, metadata)

	metadataJSON, err := MarshalVideoMetadata(metadata)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to encode normalized video metadata: %v\n", err)
		return Result{}, err
	}

	var data io.Reader
	if downloadVideo {
		file, err := os.Open(outputPath)
		if err != nil {
			fmt.Fprintf(logWriter, "Failed to open downloaded MP4: %v\n", err)
			return Result{}, err
		}
		keepTempFile = outputPath
		data = &tempVideoReader{File: file, path: outputPath, dir: filepath.Dir(tempBase)}
	}

	// Read the poster image before returning: the deferred cleanup sweeps every
	// sibling temp file except the video, so it is gone the moment we return.
	thumb := videoThumbnail(tempBase, outputPath, logWriter)

	if downloadVideo {
		fmt.Fprintf(logWriter, "Video download completed successfully\n")
	} else {
		fmt.Fprintf(logWriter, "Video metadata refresh completed successfully\n")
	}

	return Result{
		Data:        data,
		Extension:   metadata.Media.Extension,
		ContentType: metadata.Media.ContentType,
		Thumbnail:   thumb,
		Source:      "native",
		Metadata:    &Sidecar{Data: metadataJSON},
		RawMetadata: &Sidecar{Data: sanitizedRaw},
		Extras:      extras,
		// A single video is structurally one asset: --no-playlist caps the run
		// at one, and reaching here means the muxed file and both sidecars
		// exist. There is nothing else to have missed, so this is the one place
		// completeness needs no count from the extractor.
		Completeness: CompletenessComplete,
	}, nil
}

// detectedLanguageFromProbe reads the video's language off the probe output.
//
// The language is the last thing printed, on its own line, so a title
// containing newlines cannot be mistaken for it. An unusable value yields an
// empty string, which makes the subtitle filter fall back to English only.
func detectedLanguageFromProbe(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return utils.NormalizeSubtitleLang(line)
		}
	}
	return ""
}

// subtitleFileExtensions are the caption formats yt-dlp may write. VTT is asked
// for first, but a track offered only in another format is still worth storing.
var subtitleFileExtensions = []string{".vtt", ".srt", ".ttml", ".srv1", ".srv2", ".srv3", ".json3"}

// maxSubtitleArtifactBytes caps one stored track. Real caption files are tens
// of kilobytes; anything past this is a live-chat replay or a pathological
// track, and is skipped rather than allowed to bloat the archive.
const maxSubtitleArtifactBytes = 8 << 20

// collectSubtitleArtifacts loads the caption tracks yt-dlp wrote beside the
// video and turns them into storable artifacts.
//
// Returns empty results for the common case of a post with no captions. Nothing
// here can fail the archive: the video is the product, and captions are a bonus
// the platform may simply not offer.
func collectSubtitleArtifacts(tempBase string, rawInfo []byte, logWriter io.Writer) ([]ExtraArtifact, []SubtitleTrack, map[string]string) {
	matches, err := filepath.Glob(tempBase + ".*")
	if err != nil {
		return nil, nil, nil
	}
	kinds := subtitleKindsFromInfo(rawInfo)

	var extras []ExtraArtifact
	var tracks []SubtitleTrack
	contents := make(map[string]string)
	seen := make(map[string]bool)

	sort.Strings(matches)
	for _, path := range matches {
		ext := strings.ToLower(filepath.Ext(path))
		if !isSubtitleExtension(ext) {
			continue
		}
		lang := SubtitleLangFromFilename(path, tempBase)
		if lang == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if info.Size() > maxSubtitleArtifactBytes {
			fmt.Fprintf(logWriter, "Skipping oversized subtitle track %s (%d bytes)\n", filepath.Base(path), info.Size())
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(logWriter, "Could not read subtitle track %s: %v\n", filepath.Base(path), err)
			continue
		}

		format := strings.TrimPrefix(ext, ".")
		suffix := SubtitleArtifactSuffix(lang, format)
		if seen[suffix] {
			continue
		}
		seen[suffix] = true

		kind := kinds[lang]
		if kind == "" {
			kind = SubtitleKindAuto
		}
		tracks = append(tracks, SubtitleTrack{
			Lang: lang, Kind: kind, Format: format,
			ArtifactSuffix: suffix, SizeBytes: info.Size(),
		})
		extras = append(extras, ExtraArtifact{
			NameSuffix:  suffix,
			ContentType: subtitleContentType(format),
			Data:        data,
		})
		contents[suffix] = string(data)
	}
	return extras, tracks, contents
}

func isSubtitleExtension(ext string) bool {
	for _, candidate := range subtitleFileExtensions {
		if ext == candidate {
			return true
		}
	}
	return false
}

func subtitleContentType(format string) string {
	switch format {
	case "vtt":
		return "text/vtt; charset=utf-8"
	case "srt":
		return "application/x-subrip; charset=utf-8"
	case "ttml":
		return "application/ttml+xml"
	case "json3":
		return "application/json"
	default:
		return "text/plain; charset=utf-8"
	}
}

// subtitleKindsFromInfo reads which languages had a human-authored track.
//
// yt-dlp writes one file per language, preferring the manual track when both
// exist, so the info record's own "subtitles" map is what distinguishes a
// reviewed transcript from speech recognition. Languages listed only under
// automatic_captions are automatic; anything unlisted is assumed automatic,
// which is the conservative claim.
func subtitleKindsFromInfo(rawInfo []byte) map[string]string {
	kinds := make(map[string]string)
	info, err := decodeJSONObject(rawInfo)
	if err != nil {
		return kinds
	}
	for _, source := range subtitleKindSources {
		langs, ok := info[source.field].(map[string]interface{})
		if !ok {
			continue
		}
		for lang := range langs {
			kinds[lang] = source.kind
		}
	}
	return kinds
}

func logSubtitleOutcome(logWriter io.Writer, metadata *VideoMetadata) {
	if len(metadata.Subtitles) == 0 {
		fmt.Fprintf(logWriter, "No subtitles available for this video\n")
		return
	}
	for _, track := range metadata.Subtitles {
		fmt.Fprintf(logWriter, "Stored %s subtitle track %s (%s, %d bytes)\n",
			track.Kind, track.Lang, track.Format, track.SizeBytes)
	}
	if metadata.Transcript != nil {
		fmt.Fprintf(logWriter, "Derived %s transcript from the %s %s track (%d characters)\n",
			map[bool]string{true: "truncated", false: "full"}[metadata.Transcript.Truncated],
			metadata.Transcript.Source, metadata.Transcript.Lang, len(metadata.Transcript.Text))
	}
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
		// Keep the complete extractor result as an auditable sidecar. Arker
		// sanitizes it before it ever reaches durable storage or an API response.
		"--write-info-json",
		"--no-clean-infojson",
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"--verbose",
		"-o", outputTemplate,
	}
}

func findYtDlpInfoJSON(tempBase string) (string, error) {
	matches, err := filepath.Glob(tempBase + "*.info.json")
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("yt-dlp did not write an info JSON sidecar")
	}
	return matches[0], nil
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

	// Keep the platform's actual poster. It is already authored and framed for
	// this video; cropping it to Arker's old 16:9 JPEG tile loses both the image
	// and its intrinsic dimensions.
	t, err := thumbnail.OriginalFromReader(f)
	if err != nil {
		fmt.Fprintf(logWriter, "Thumbnail capture skipped: %v\n", err)
		return nil
	}

	fmt.Fprintf(logWriter, "Thumbnail captured from %s: %dx%d, %d bytes\n",
		filepath.Base(path), t.Width, t.Height, len(t.Data))
	return &Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialPreview}
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
	// The unclean yt-dlp info JSON can contain request headers and signed media
	// URLs before Arker sanitizes it. Keep every output in a 0700 directory so
	// no other local user can read the transient raw record.
	dir, err := os.MkdirTemp("", "arker-video-*")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "video"), nil
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
	if keep == "" {
		_ = os.Remove(filepath.Dir(tempBase))
	}
}
