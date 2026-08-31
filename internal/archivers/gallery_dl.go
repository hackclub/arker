package archivers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"

	"arker/internal/thumbnail"
	"arker/internal/utils"
)

// GalleryDLArchiver downloads every media file behind a "post" URL using
// gallery-dl, the image-world counterpart to yt-dlp. It covers Instagram,
// X/Twitter, Reddit, Tumblr, Bluesky, Flickr, Imgur, Pixiv and ~300 other
// sites, and unlike yt-dlp it handles photo posts and mixed photo/video
// carousels instead of failing with "There is no video in this post".
//
// The output is a ZIP containing every downloaded file, gallery-dl's raw
// per-file metadata sidecars, and a normalized metadata.json written by Arker.
type GalleryDLArchiver struct{}

// GalleryMetadataSchemaVersion is the public normalized gallery/post contract,
// covering both this struct and the /gallery/:shortid/manifest envelope that
// carries it. Additive fields do not require a new version; incompatible
// meaning or shape changes do. It shares the video contract's numbering so a
// consumer reading either manifest sees one versioning scheme.
const GalleryMetadataSchemaVersion = "1"

// GalleryMetadata is Arker's normalized view of a gallery-dl capture. The
// raw, site-specific metadata is preserved alongside it in the ZIP, so this
// only needs to carry the fields a viewer wants: who posted, what they said,
// when, and what files came back.
type GalleryMetadata struct {
	SourceURL   string `json:"source_url"`
	Extractor   string `json:"extractor,omitempty"`
	Subcategory string `json:"subcategory,omitempty"`
	PostID      string `json:"post_id,omitempty"`
	PostURL     string `json:"post_url,omitempty"`
	Author      string `json:"author,omitempty"`
	AuthorName  string `json:"author_name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Date        string `json:"date,omitempty"`
	Likes       *int64 `json:"likes,omitempty"`
	// Views and Comments complete the engagement picture where the extractor
	// exposes them. Like Likes they are pointers: a missing count is a fact
	// about the source, not a zero.
	Views     *int64        `json:"views,omitempty"`
	Comments  *int64        `json:"comments,omitempty"`
	Tags      []string      `json:"tags,omitempty"`
	FileCount int           `json:"file_count"`
	Files     []GalleryFile `json:"files"`
	// Completeness answers whether FileCount is all of the post or only what
	// survived. gallery-dl keeps partial downloads, so file_count alone cannot
	// distinguish a 3-slide post from 3 slides of a 10-slide carousel. Nil on
	// archives written before this was recorded, which reads as unknown.
	Completeness *Completeness `json:"completeness,omitempty"`
	ToolVersion  string        `json:"gallery_dl_version,omitempty"`
	ArchivedAt   string        `json:"archived_at"`
}

// GalleryFile describes one downloaded media file inside the ZIP.
type GalleryFile struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	IsVideo     bool   `json:"is_video"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	// DurationSeconds is the playable length of a video slide, read off the
	// stored bytes by ffprobe at archive time (ProbeGalleryVideoFiles). It is
	// what lets a single-video post archived through the gallery flow serve a
	// video manifest with a real duration. Nil for stills and for bundles
	// written before this was recorded.
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	// AltText is the poster's own description of this image, where the
	// extractor exposes it. It is part of the post as published — often the
	// only text describing an image-only post — so it is worth keeping.
	AltText      string `json:"alt_text,omitempty"`
	MetadataFile string `json:"metadata_file,omitempty"`
}

// galleryMetadataFilename is the normalized metadata Arker adds at the ZIP
// root. gallery-dl is configured to emit numeric filenames (001.jpg), so this
// name can never collide with a downloaded media file.
const galleryMetadataFilename = "metadata.json"

func (a *GalleryDLArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (Result, error) {
	fmt.Fprintf(logWriter, "Starting gallery archive for: %s\n", url)

	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}

	// gallery-dl only reads the jar (there is a separate --cookies-export for
	// writing), but reuse the per-run private copy anyway: it keeps a
	// read-only mounted secret safe and matches how yt-dlp is invoked.
	cookieArgs, cleanupCookies, err := utils.MediaCookieArgsForRun()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to prepare gallery-dl cookies: %v\n", err)
		return Result{}, err
	}
	defer cleanupCookies()
	if len(cookieArgs) == 0 {
		fmt.Fprintf(logWriter, "No cookies configured; logged-in-only sites (e.g. Instagram) will fail\n")
	}

	version, versionErr := utils.GalleryDlVersion(ctx)
	if versionErr == nil {
		fmt.Fprintf(logWriter, "gallery-dl version: %s\n", version)
	} else {
		fmt.Fprintf(logWriter, "Could not determine gallery-dl version: %v\n", versionErr)
	}

	tmpDir, err := os.MkdirTemp("", "arker-gallery-*")
	if err != nil {
		return Result{}, fmt.Errorf("failed to create temp directory: %w", err)
	}
	// Cleanup is handed to the ZIP goroutine on the success path; every early
	// return below removes the directory itself.
	cleanupTmp := func() { _ = os.RemoveAll(tmpDir) }
	success := false
	defer func() {
		if !success {
			cleanupTmp()
		}
	}()

	redactedLog := utils.NewRedactingWriter(logWriter, utils.MediaProxyRedactionSecrets())

	args := galleryDlDownloadArgs(tmpDir)
	args = append(args, cookieArgs...)
	args = append(args, utils.MediaProxyArgs()...)
	args = append(args, utils.GalleryDlUserAgentArgs()...)
	args = append(args, utils.GalleryDlSleepArgs()...)
	args = append(args, url)

	// Watch for the one failure that turns a video post into an empty archive
	// with an unhelpful exit code (see galleryDlYtdlImportError).
	ytdlWatch := newPhraseWatcher(redactedLog, galleryDlYtdlImportError)

	cmd := exec.CommandContext(ctx, "gallery-dl")
	cmd.Args = append(cmd.Args, args...)
	cmd.Stdout = ytdlWatch
	cmd.Stderr = ytdlWatch
	// Own process group so a timeout kills the whole tree, not just the parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	fmt.Fprintf(logWriter, "Starting gallery-dl download process...\n")
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start gallery-dl: %v\n", err)
		return Result{}, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				fmt.Fprintf(logWriter, "Context cancelled, killing gallery-dl process group\n")
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	runErr := cmd.Wait()

	media, sidecars, err := collectGalleryFiles(tmpDir)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to inspect gallery-dl output: %v\n", err)
		return Result{}, err
	}
	media, sidecars, err = canonicalizeGalleryFiles(tmpDir, media, sidecars, logWriter)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to normalize gallery-dl output: %v\n", err)
		return Result{}, err
	}

	// gallery-dl exits non-zero for partial failures too, so a run that still
	// produced media is worth keeping. Only fail when nothing came back.
	if len(media) == 0 {
		if ytdlWatch.Seen() {
			fmt.Fprintf(logWriter, "%s\n", galleryDlMissingYtdlMessage)
			return Result{}, fmt.Errorf("%s", galleryDlMissingYtdlMessage)
		}
		if runErr != nil {
			fmt.Fprintf(logWriter, "gallery-dl failed: %v (%s)\n", runErr, describeGalleryDlExit(runErr))
			return Result{}, fmt.Errorf("gallery-dl failed: %w (%s)", runErr, describeGalleryDlExit(runErr))
		}
		fmt.Fprintf(logWriter, "gallery-dl downloaded no files for %s\n", url)
		return Result{}, fmt.Errorf("gallery-dl downloaded no files for %s", url)
	}
	if ytdlWatch.Seen() {
		// Some media came back, so the archive is worth keeping, but a video
		// slide is either missing or downgraded. Say so where an operator will
		// see it rather than letting the capture look complete.
		fmt.Fprintf(logWriter, "WARNING: %s\n", galleryDlMissingYtdlMessage)
	}
	if runErr != nil {
		fmt.Fprintf(logWriter, "gallery-dl exited with %v (%s) but produced %d file(s); keeping partial archive\n",
			runErr, describeGalleryDlExit(runErr), len(media))
	}

	// A kept partial download is the whole reason this exists: without a
	// recorded completeness the stored archive claims to be the post, and a
	// 3-of-10 carousel reads exactly like a 3-slide one.
	completeness := galleryCompleteness(tmpDir, media, sidecars, runErr != nil, logWriter)
	// Some post shapes are structurally single-asset (a Flickr photo page, a
	// Pinterest pin): when the extractor exposes no count field, the URL shape
	// itself supplies expected=1, so those posts are not condemned to
	// unknown-completeness forever.
	if completeness.State == CompletenessUnknown && len(media) == 1 && utils.StructurallySingleAssetGalleryURL(url) {
		one := 1
		completeness = CompletenessFromCounts(&one, len(media), runErr != nil)
		fmt.Fprintf(logWriter, "Completeness: post shape is structurally single-asset; expected count is 1 by construction\n")
	}
	logGalleryCompleteness(logWriter, completeness)

	metadata := buildGalleryMetadata(tmpDir, url, version, media, sidecars, logWriter)
	metadata.Completeness = &completeness
	// The files are still on disk here; once zipped they are behind an
	// immutable stored object, so this is the only chance to read intrinsic
	// video facts (duration, dimensions) off the actual bytes.
	ProbeGalleryVideoFiles(ctx, tmpDir, metadata.Files, logWriter)
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to encode gallery metadata: %v\n", err)
		return Result{}, fmt.Errorf("failed to encode gallery metadata: %w", err)
	}

	fmt.Fprintf(logWriter, "Downloaded %d file(s) from %s\n", len(media), metadata.Extractor)
	if metadata.Author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", metadata.Author)
	}
	if metadata.Description != "" {
		fmt.Fprintf(logWriter, "Caption: %s\n", utils.TruncateForLog(metadata.Description, 300))
	}

	// Build the thumbnail before the ZIP goroutine starts. That goroutine owns
	// cleanupTmp, so once it is running the downloaded files can vanish at any
	// moment; reading them here is the only safe window.
	thumb := galleryThumbnail(tmpDir, metadata, logWriter)

	// Stream the ZIP so a large carousel never has to sit in memory.
	pipeReader, pipeWriter := io.Pipe()
	success = true
	go func() {
		defer pipeWriter.Close()
		defer cleanupTmp()

		zipWriter := zip.NewWriter(pipeWriter)
		if err := writeGalleryZip(zipWriter, tmpDir, metadataJSON, logWriter); err != nil {
			fmt.Fprintf(logWriter, "Error building gallery ZIP: %v\n", err)
			_ = zipWriter.Close()
			pipeWriter.CloseWithError(err)
			return
		}
		if err := zipWriter.Close(); err != nil {
			fmt.Fprintf(logWriter, "Error finalizing gallery ZIP: %v\n", err)
			pipeWriter.CloseWithError(err)
			return
		}
		fmt.Fprintf(logWriter, "Successfully created gallery ZIP archive\n")
	}()

	return Result{
		Data:         pipeReader,
		Extension:    ".zip",
		ContentType:  "application/zip",
		Thumbnail:    thumb,
		Completeness: completeness.State,
	}, nil
}

// galleryCompleteness decides whether the files on disk are the whole post.
//
// gallery-dl exits non-zero for a partial download and the archiver keeps that
// output, so the only way to tell a complete post from a salvaged one is to
// compare what landed against what the extractor said was there.
func galleryCompleteness(dir string, media []string, sidecars map[string]string, runFailed bool, logWriter io.Writer) Completeness {
	expected := galleryExpectedCount(dir, media, sidecars, logWriter)
	result := CompletenessFromCounts(expected, len(media), runFailed)
	if result.State == CompletenessPartial && expected != nil {
		result.MissingIndices = galleryMissingIndices(*expected, media)
	}
	return result
}

func logGalleryCompleteness(logWriter io.Writer, completeness Completeness) {
	switch {
	case completeness.Expected != nil:
		fmt.Fprintf(logWriter, "Completeness: %s (%d of %d expected file(s) stored)\n",
			completeness.State, completeness.Stored, *completeness.Expected)
	case completeness.State == CompletenessPartial:
		fmt.Fprintf(logWriter, "Completeness: partial (%d file(s) stored; the run reported a failure and the post's file count is unknown)\n",
			completeness.Stored)
	default:
		fmt.Fprintf(logWriter, "Completeness: unknown (%d file(s) stored; no extractor field reported how many the post has)\n",
			completeness.Stored)
	}
	if len(completeness.MissingIndices) > 0 {
		fmt.Fprintf(logWriter, "Missing slide(s): %v\n", completeness.MissingIndices)
	}
}

// galleryExpectedCountKeys name the field an extractor uses for "how many files
// this post has". gallery-dl's own convention is "count" (it is what the
// {count} format key reads), but extractors that predate it, or that model a
// post as an album, use their own name. Order matters only in that the generic
// key is checked first; galleryInt also searches the container objects, which
// is what finds Imgur's album.image_count.
var galleryExpectedCountKeys = []string{
	"count",
	"media_count",
	"image_count",
	"page_count",
	"photo_count",
	"carousel_media_count",
}

// maxGalleryExpectedCount rejects an implausible value. These keys are resolved
// by name across ~300 extractors, so a site that uses one of them for something
// else (a follower count, a view count) would otherwise mark every capture
// partial forever. A post with more media than this does not exist on the sites
// Arker archives.
const maxGalleryExpectedCount = 1000

// galleryExpectedCount reads the post's file count out of the sidecars.
//
// gallery-dl merges post-level metadata into every file's dict, so any sidecar
// carries it — which matters, because the sidecar of a slide that failed to
// download does not exist. Scanning in order finds the count from whichever
// slide did make it.
func galleryExpectedCount(dir string, media []string, sidecars map[string]string, logWriter io.Writer) *int {
	for _, name := range media {
		sidecar, ok := sidecars[name]
		if !ok {
			continue
		}
		raw := readGalleryJSON(filepath.Join(dir, sidecar), logWriter)
		if raw == nil {
			continue
		}
		if count := galleryCountFrom(raw); count != nil {
			return count
		}
	}
	return nil
}

func galleryCountFrom(raw map[string]interface{}) *int {
	value := galleryInt(raw, galleryExpectedCountKeys...)
	if value == nil || *value <= 0 || *value > maxGalleryExpectedCount {
		return nil
	}
	count := int(*value)
	return &count
}

// galleryMissingIndices names which slides are gone.
//
// The archiver forces "{num:>03}.{extension}" filenames, so the stored names
// carry gallery-dl's own 1-based slide numbers and a gap in them is the missing
// slide. Returns nil when the names do not parse, rather than reporting every
// index as missing.
func galleryMissingIndices(expected int, media []string) []int {
	present := make(map[int]bool, len(media))
	for _, name := range media {
		if index := galleryFileIndex(name); index > 0 {
			present[index] = true
		}
	}
	if len(present) == 0 {
		return nil
	}
	var missing []int
	for index := 1; index <= expected && len(missing) < maxCompletenessMissingIndices; index++ {
		if !present[index] {
			missing = append(missing, index)
		}
	}
	return missing
}

// galleryFileIndex reads the leading slide number off a stored filename.
// Returns 0 for any name that is not numbered, so a Bright Data or legacy
// layout degrades to "cannot tell which are missing" instead of guessing.
func galleryFileIndex(name string) int {
	base := name
	if dot := strings.Index(base, "."); dot >= 0 {
		base = base[:dot]
	}
	if base == "" {
		return 0
	}
	index := 0
	for _, r := range base {
		if r < '0' || r > '9' {
			return 0
		}
		index = index*10 + int(r-'0')
		if index > maxGalleryExpectedCount {
			return 0
		}
	}
	return index
}

// galleryThumbnail previews a post using its first still image.
//
// Slide 1 is what the post leads with, so it is the natural cover. Videos are
// skipped because gallery-dl stores the media itself, not a poster frame, and
// decoding a video here is out of scope; a carousel that opens with a video
// falls back to its first photo, and an all-video post gets no thumbnail.
//
// Returns nil on any failure. A post with no usable cover is not an error.
func galleryThumbnail(dir string, metadata *GalleryMetadata, logWriter io.Writer) *Thumbnail {
	if metadata == nil {
		return nil
	}
	for _, file := range metadata.Files {
		if file.IsVideo || file.Name == "" {
			continue
		}
		if !strings.HasPrefix(file.ContentType, "image/") {
			continue
		}

		f, err := os.Open(filepath.Join(dir, file.Name))
		if err != nil {
			fmt.Fprintf(logWriter, "Could not open %s for thumbnail: %v\n", file.Name, err)
			continue
		}
		// A post image is already the real, authored preview. Preserve its bytes,
		// format, dimensions and aspect ratio rather than turning it into an
		// Arker-framed 16:9 JPEG.
		t, err := thumbnail.OriginalFromReader(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(logWriter, "Thumbnail from %s skipped: %v\n", file.Name, err)
			continue
		}

		fmt.Fprintf(logWriter, "Thumbnail captured from %s: %dx%d, %d bytes\n",
			file.Name, t.Width, t.Height, len(t.Data))
		return &Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height}
	}

	fmt.Fprintf(logWriter, "No still image available for a thumbnail\n")
	return nil
}

// galleryDlDownloadArgs builds the invariant part of the gallery-dl command
// line. Output is forced flat with zero-padded numeric names so the ZIP layout
// is identical for every site and can never collide with metadata.json.
func galleryDlDownloadArgs(destDir string) []string {
	return []string{
		// Never pick up a gallery-dl.conf from the host: the archiver's
		// behavior must depend only on what Arker passes.
		"--config-ignore",
		// -D (not -d) sets the destination AND clears the per-extractor
		// subdirectory template, giving one flat directory per job.
		"-D", destDir,
		"-f", "{num:>03}.{extension}",
		"--write-metadata",
		// gallery-dl rewrites its --cookies file on exit by default, dumping
		// every cookie it picked up along the way (CDN hosts included) into
		// the jar. The run gets a private copy so the configured secret is
		// never touched, but there is no reason to pay for the rewrite.
		"-o", "cookies-update=false",
		// Reddit hosts v.redd.it videos as DASH with the audio in a SEPARATE
		// stream, so whatever downloads them has to mux. gallery-dl always
		// delegates that to yt-dlp (imported as a Python module, "ytdl:" URLs);
		// the only question is what it hands over.
		//
		// The default, extractor.reddit.videos=dash, passes the DASHPlaylist.mpd
		// and lets gallery-dl's ytdl downloader parse it. That works only as far
		// as the manifest goes: when reddit's mpd lists no audio adaptation set,
		// the archive silently ends up video-only.
		//
		// "ytdl" hands yt-dlp the submission permalink instead, so yt-dlp's own
		// reddit extractor builds the format list — the DASH manifest AND the
		// HLS ladder AND the video-only fallback_url (yt-dlp 2026.08.04
		// extractor/reddit.py:396-441). Format selection then merges the best
		// video with the best audio via ffmpeg. It costs one extra reddit
		// request per video post and is the only route that reliably stores
		// audio.
		"-o", "extractor.reddit.videos=ytdl",
		"--no-part",
		"-R", "3",
		"--http-timeout", "30",
		// Deliberately not --verbose. Default output already names every file
		// written and prints a one-line "[site][error] ..." on failure, which
		// is what the archive log and the failure pane need; verbose adds
		// urllib3 chatter and a full Python traceback around it.
	}
}

// galleryDlYtdlImportError is the line gallery-dl's ytdl downloader logs when
// yt-dlp is not importable from gallery-dl's own Python interpreter
// (gallery-dl 1.32.9 downloader/ytdl.py:56-63). Everything routed through a
// "ytdl:" URL — reddit videos, Instagram DASH manifests, TikTok audio — then
// downloads nothing, and the only other trace is a generic exit code 4.
const galleryDlYtdlImportError = "Cannot import yt-dlp or youtube-dl"

// galleryDlMissingYtdlMessage explains that failure in terms of the fix. The
// two tools must live in the SAME Python environment: gallery-dl imports
// yt-dlp as a module, so a separate pipx/uv install of yt-dlp is invisible to
// it no matter what the shell PATH says.
const galleryDlMissingYtdlMessage = "gallery-dl could not import yt-dlp, so it downloaded no video for this post " +
	"(reddit videos, Instagram DASH manifests and TikTok audio all go through gallery-dl's ytdl downloader). " +
	"Install yt-dlp into the same Python environment as gallery-dl."

// phraseWatcher passes writes through unchanged while reporting whether a
// phrase ever appeared in the stream.
//
// Process output arrives in arbitrary chunks, so it carries the tail of each
// write forward: a phrase split across two reads still matches.
type phraseWatcher struct {
	out    io.Writer
	phrase []byte
	tail   []byte
	seen   bool
	mu     sync.Mutex
}

func newPhraseWatcher(out io.Writer, phrase string) *phraseWatcher {
	return &phraseWatcher{out: out, phrase: []byte(phrase)}
}

func (w *phraseWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.seen {
		window := append(w.tail, p...)
		if bytes.Contains(window, w.phrase) {
			w.seen = true
			w.tail = nil
		} else if overlap := len(w.phrase) - 1; overlap > 0 && len(window) > overlap {
			w.tail = append([]byte(nil), window[len(window)-overlap:]...)
		} else {
			w.tail = append([]byte(nil), window...)
		}
	}
	w.mu.Unlock()

	return w.out.Write(p)
}

func (w *phraseWatcher) Seen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// galleryDlExitReasons maps gallery-dl's exit bits to human-readable causes.
//
// The exit status is a bitmask OR'd across every URL and extractor in the
// invocation, not an enum, so a single run can report several of these at once.
// The values are not documented upstream; they come from reading
// gallery_dl/exception.py and were confirmed against live runs.
var galleryDlExitReasons = []struct {
	bit    int
	reason string
}{
	{1, "unexpected error"},
	{4, "extraction or download failed (404, redirect to login, rate limit)"},
	{8, "anti-bot challenge (e.g. Cloudflare)"},
	{16, "authentication required or rejected"},
	{32, "bad input (format string, filter, or input file)"},
	{64, "no gallery-dl extractor supports this URL"},
	{128, "local filesystem error"},
}

// describeGalleryDlExit turns a gallery-dl exit status into something a human
// reading archive logs can act on.
func describeGalleryDlExit(err error) string {
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return "gallery-dl did not run"
	}

	code := exitErr.ExitCode()
	var reasons []string
	for _, entry := range galleryDlExitReasons {
		if code&entry.bit != 0 {
			reasons = append(reasons, entry.reason)
		}
	}
	if len(reasons) == 0 {
		return fmt.Sprintf("gallery-dl exit code %d", code)
	}
	return fmt.Sprintf("%s (exit code %d)", strings.Join(reasons, "; "), code)
}

// collectGalleryFiles splits gallery-dl's flat output directory into media
// files and their JSON sidecars.
func collectGalleryFiles(dir string) (media []string, sidecars map[string]string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}

	sidecars = make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".json") {
			// gallery-dl names sidecars "<media file>.json".
			sidecars[strings.TrimSuffix(name, ".json")] = name
			continue
		}
		media = append(media, name)
	}

	sort.Strings(media)
	return media, sidecars, nil
}

// canonicalizeGalleryFiles makes the filename describe the bytes that were
// actually downloaded. Some providers label JPEG responses as HEIC; keeping
// that name would make metadata and HTTP serving disagree with the payload.
// A matching gallery-dl sidecar moves with its media file so every internal
// reference remains valid.
func canonicalizeGalleryFiles(dir string, media []string, sidecars map[string]string, logWriter io.Writer) ([]string, map[string]string, error) {
	canonicalMedia := make([]string, 0, len(media))
	canonicalSidecars := make(map[string]string, len(sidecars))

	for _, name := range media {
		canonicalName, _, err := InspectGalleryMediaFile(dir, name)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect %s: %w", name, err)
		}

		sidecar := sidecars[name]
		canonicalSidecar := sidecar
		if sidecar != "" && canonicalName != name {
			canonicalSidecar = canonicalName + ".json"
		}

		if canonicalName != name {
			if err := galleryRenameAvailable(dir, name, canonicalName); err != nil {
				return nil, nil, err
			}
			if sidecar != "" {
				if err := galleryRenameAvailable(dir, sidecar, canonicalSidecar); err != nil {
					return nil, nil, err
				}
			}

			if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, canonicalName)); err != nil {
				return nil, nil, fmt.Errorf("rename %s to %s: %w", name, canonicalName, err)
			}
			if sidecar != "" {
				if err := os.Rename(filepath.Join(dir, sidecar), filepath.Join(dir, canonicalSidecar)); err != nil {
					_ = os.Rename(filepath.Join(dir, canonicalName), filepath.Join(dir, name))
					return nil, nil, fmt.Errorf("rename %s to %s: %w", sidecar, canonicalSidecar, err)
				}
			}
			fmt.Fprintf(logWriter, "Normalized gallery media filename %s to %s based on its bytes\n", name, canonicalName)
		}

		canonicalMedia = append(canonicalMedia, canonicalName)
		if canonicalSidecar != "" {
			canonicalSidecars[canonicalName] = canonicalSidecar
		}
	}

	sort.Strings(canonicalMedia)
	return canonicalMedia, canonicalSidecars, nil
}

func galleryRenameAvailable(dir, oldName, newName string) error {
	oldInfo, err := os.Stat(filepath.Join(dir, oldName))
	if err != nil {
		return err
	}
	newInfo, err := os.Stat(filepath.Join(dir, newName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(oldInfo, newInfo) {
		return nil
	}
	return fmt.Errorf("cannot rename %s to %s: destination already exists", oldName, newName)
}

// buildGalleryMetadata normalizes gallery-dl's site-specific metadata into the
// handful of fields a viewer actually needs. Every site names things
// differently, so each field is resolved from a priority list of known keys
// and simply omitted when nothing matches.
func buildGalleryMetadata(dir, sourceURL, version string, media []string, sidecars map[string]string, logWriter io.Writer) *GalleryMetadata {
	meta := &GalleryMetadata{
		SourceURL:   sourceURL,
		FileCount:   len(media),
		ToolVersion: version,
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	// Read the post record off the first media file's sidecar. gallery-dl
	// merges the post-level metadata into every file's dict, so slide 1
	// carries the caption, author, and date for the whole post.
	var raw map[string]interface{}
	if len(media) > 0 {
		if sidecar, ok := sidecars[media[0]]; ok {
			raw = readGalleryJSON(filepath.Join(dir, sidecar), logWriter)
		}
	}

	if raw != nil {
		meta.Extractor = galleryString(raw, "category")
		meta.Subcategory = galleryString(raw, "subcategory")

		// "id" and "url" are ambiguous: at the top level they name the
		// individual file (Imgur's image id and its CDN link), while the
		// enclosing album/post object holds the ones a viewer wants. Prefer an
		// explicit post-level key, then the container, and only then fall back.
		meta.PostID = firstNonEmpty(
			galleryString(raw, "post_id", "tweet_id", "post_shortcode", "shortcode"),
			galleryNested(raw, "id"),
			galleryString(raw, "id"),
		)
		meta.PostURL = firstNonEmpty(
			galleryString(raw, "post_url", "webpage_url"),
			galleryNested(raw, "url"),
		)

		meta.Author, meta.AuthorName = resolveGalleryAuthor(raw)

		meta.Title = firstNonEmpty(galleryString(raw, "title"), galleryNested(raw, "title"))

		// Caption key order matters. Bluesky uses "text" for the post body and
		// "description" for an image's alt text, so checking description first
		// would surface alt text as the caption. Instagram has no "text" key,
		// so it still resolves to "description".
		meta.Description = firstNonEmpty(
			galleryString(raw, "text", "content", "caption"),
			galleryString(raw, "description"),
			galleryNested(raw, "description"),
		)

		meta.Date = galleryString(raw, "post_date", "date", "created_at", "taken_at")
		// Every site counts approval differently: likes, hearts, upvotes,
		// favorites. Treat them as one number rather than modelling each.
		//
		// Order is by closeness to "likes", not convenience: a site can expose
		// several of these at once and the first match wins. Imgur carries both
		// upvote_count (366) and favorite_count (0) on the same album, so
		// checking favourites first would report zero likes on a popular post.
		meta.Likes = galleryInt(raw,
			"likes", "like_count", "likeCount",
			"upvote_count", "point_count",
			"favorite_count", "favorites", "score")
		// Views and comments follow the same first-match-wins rule. The keys
		// are the ones extractors actually emit; a site that models comments
		// as a list rather than a count simply resolves to nil, because
		// galleryInt only accepts numbers.
		meta.Views = galleryInt(raw,
			"view_count", "views", "video_view_count",
			"play_count", "video_play_count")
		meta.Comments = galleryInt(raw,
			"comment_count", "comments", "num_comments",
			"reply_count", "replies")
		meta.Tags = galleryStrings(raw, "tags", "hashtags")
	}

	for _, name := range media {
		file := GalleryFile{
			Name:         name,
			ContentType:  galleryContentType(name),
			MetadataFile: sidecars[name],
		}
		file.IsVideo = strings.HasPrefix(file.ContentType, "video/")
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			file.Size = info.Size()
		}
		if sidecar, ok := sidecars[name]; ok {
			if perFile := readGalleryJSON(filepath.Join(dir, sidecar), logWriter); perFile != nil {
				if w := galleryInt(perFile, "width"); w != nil {
					file.Width = int(*w)
				}
				if h := galleryInt(perFile, "height"); h != nil {
					file.Height = int(*h)
				}
				file.AltText = galleryAltText(perFile, meta.Extractor)
			}
		}
		meta.Files = append(meta.Files, file)
	}

	return meta
}

// galleryAltText reads the poster's description of one image.
//
// The explicit keys are unambiguous wherever an extractor provides them. The
// Bluesky special case is not: it stores the post body under "text" and the
// image's alt text under "description", so "description" is alt text there and
// the caption everywhere else — reading it generically would stamp Instagram's
// caption onto every slide as if it were alt text.
func galleryAltText(perFile map[string]interface{}, extractor string) string {
	if alt := galleryString(perFile, "alt_text", "ext_alt_text", "alt", "altText", "media_alt", "accessibility_caption"); alt != "" {
		return alt
	}
	if strings.EqualFold(extractor, "bluesky") {
		return galleryString(perFile, "description")
	}
	return ""
}

func readGalleryJSON(path string, logWriter io.Writer) map[string]interface{} {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	// UseNumber keeps large integers exact. Post IDs on X and Instagram exceed
	// 2^53, so decoding them as float64 would silently corrupt the value
	// recorded in metadata.json (1234567890123456789 -> ...456768).
	decoder := json.NewDecoder(file)
	decoder.UseNumber()

	var parsed map[string]interface{}
	if err := decoder.Decode(&parsed); err != nil {
		fmt.Fprintf(logWriter, "Could not parse %s: %v\n", filepath.Base(path), err)
		return nil
	}
	return parsed
}

// galleryContainers are objects extractors nest post-level metadata under.
// gallery-dl merges the post record into every file's dict, but some sites keep
// it in a sub-object instead: Imgur puts the album's title, URL and vote counts
// under "album" while the top level describes just the one image.
var galleryContainers = []string{"album", "post", "gallery", "tweet", "submission", "record"}

// galleryPersonKeys are the objects that hold author details when an extractor
// models the poster as a nested record rather than flat fields.
var galleryPersonKeys = []string{"author", "user", "account", "owner", "uploader", "artist"}

// galleryString returns the first key holding a non-empty string, searching the
// top level only.
func galleryString(raw map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			// Numeric IDs keep their exact source digits.
			return value.String()
		}
	}
	return ""
}

// galleryNested runs the same lookup against the known container objects only,
// so a caller can decide whether the container or the top level should win.
func galleryNested(raw map[string]interface{}, keys ...string) string {
	for _, container := range galleryContainers {
		nested, ok := raw[container].(map[string]interface{})
		if !ok {
			continue
		}
		if found := galleryString(nested, keys...); found != "" {
			return found
		}
	}
	return ""
}

// galleryObject returns a nested object by key, looking at the top level first
// and then inside the container objects.
func galleryObject(raw map[string]interface{}, key string) map[string]interface{} {
	if object, ok := raw[key].(map[string]interface{}); ok {
		return object
	}
	for _, container := range galleryContainers {
		nested, ok := raw[container].(map[string]interface{})
		if !ok {
			continue
		}
		if object, ok := nested[key].(map[string]interface{}); ok {
			return object
		}
	}
	return nil
}

// resolveGalleryAuthor pulls a handle and a display name out of whichever shape
// the extractor uses: flat fields (Instagram's username/fullname) or a nested
// person object (Bluesky's author{handle,displayName}, Imgur's album.account).
func resolveGalleryAuthor(raw map[string]interface{}) (handle, display string) {
	handle = galleryString(raw, "username", "uploader", "artist", "owner")
	display = galleryString(raw, "fullname", "full_name", "author_name", "displayName", "display_name")

	for _, key := range galleryPersonKeys {
		if handle != "" && display != "" {
			break
		}
		person := galleryObject(raw, key)
		if person == nil {
			// Some extractors store the author as a bare string.
			if handle == "" {
				handle = galleryString(raw, key)
			}
			continue
		}
		if handle == "" {
			handle = galleryString(person, "username", "handle", "name", "login", "nick")
		}
		if display == "" {
			display = galleryString(person, "displayName", "display_name", "fullname", "full_name", "name")
		}
	}

	// A single name is a handle, not a display name.
	if handle == "" && display != "" {
		handle, display = display, ""
	}
	// Don't render "someone (someone)".
	if handle == display {
		display = ""
	}
	return handle, display
}

// galleryInt returns the first key holding an integer, searching the top level
// then the container objects.
func galleryInt(raw map[string]interface{}, keys ...string) *int64 {
	if found := galleryIntIn(raw, keys...); found != nil {
		return found
	}
	for _, container := range galleryContainers {
		nested, ok := raw[container].(map[string]interface{})
		if !ok {
			continue
		}
		if found := galleryIntIn(nested, keys...); found != nil {
			return found
		}
	}
	return nil
}

func galleryIntIn(raw map[string]interface{}, keys ...string) *int64 {
	for _, key := range keys {
		number, ok := raw[key].(json.Number)
		if !ok {
			continue
		}
		result, err := number.Int64()
		if err != nil {
			continue
		}
		return &result
	}
	return nil
}

// firstNonEmpty returns the first non-empty candidate.
func firstNonEmpty(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func galleryStrings(raw map[string]interface{}, keys ...string) []string {
	for _, key := range keys {
		values, ok := raw[key].([]interface{})
		if !ok {
			continue
		}
		var result []string
		for _, value := range values {
			if str, ok := value.(string); ok && strings.TrimSpace(str) != "" {
				result = append(result, strings.TrimSpace(str))
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return nil
}

// galleryContentType maps a downloaded filename to a MIME type. mime's table
// misses a few formats social sites actually serve, so those are pinned here.
func galleryContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".json":
		return "application/json"
	}
	if byExt := mime.TypeByExtension(filepath.Ext(name)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

// InspectGalleryMediaFile detects a gallery asset's actual media type from its
// leading bytes and returns the canonical filename and MIME type. Unknown
// formats retain their original extension-based type rather than guessing.
func InspectGalleryMediaFile(dir, name string) (canonicalName, contentType string, err error) {
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	header := make([]byte, 512)
	n, err := file.Read(header)
	if err != nil && err != io.EOF {
		return "", "", err
	}
	contentType, extension := galleryMediaType(name, header[:n])
	canonicalName = strings.TrimSuffix(name, filepath.Ext(name)) + extension
	return canonicalName, contentType, nil
}

// GalleryMediaContentType detects the type of a ZIP entry from its leading
// bytes, falling back to its filename for legacy or unrecognized formats.
// Handlers use this for archives written before filenames were canonicalized.
func GalleryMediaContentType(name string, header []byte) string {
	contentType, _ := galleryMediaType(name, header)
	return contentType
}

func galleryMediaType(name string, header []byte) (contentType, extension string) {
	if detected, extension, ok := galleryISOBaseMediaType(header); ok {
		return detected, extension
	}
	detected := http.DetectContentType(header)
	if semicolon := strings.IndexByte(detected, ';'); semicolon >= 0 {
		detected = detected[:semicolon]
	}
	if extension, ok := galleryCanonicalExtensions[detected]; ok {
		return detected, extension
	}

	return galleryContentType(name), filepath.Ext(name)
}

var galleryCanonicalExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"audio/mpeg":      ".mp3",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
}

// galleryISOBaseMediaType covers the image brands net/http does not sniff.
// AVIF, HEIC, and MP4 all start with an ISO BMFF ftyp box, so all compatible
// brands are checked and image brands win over a generic container brand.
func galleryISOBaseMediaType(header []byte) (contentType, extension string, ok bool) {
	if len(header) < 12 || string(header[4:8]) != "ftyp" {
		return "", "", false
	}
	boxSize := int(uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3]))
	if boxSize < 12 || boxSize > len(header) {
		boxSize = len(header)
	}
	brands := make(map[string]bool)
	for offset := 8; offset+4 <= boxSize; offset += 4 {
		brands[string(header[offset:offset+4])] = true
	}
	if brands["avif"] || brands["avis"] {
		return "image/avif", ".avif", true
	}
	for _, brand := range []string{"heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs"} {
		if brands[brand] {
			return "image/heic", ".heic", true
		}
	}
	if brands["mif1"] || brands["msf1"] {
		return "image/heif", ".heif", true
	}
	return "", "", false
}

// writeGalleryZip stores Arker's metadata.json first so a reader can stream
// the header without buffering the whole archive, then every gallery-dl file.
// Media is stored uncompressed: JPEG/MP4 payloads do not deflate meaningfully
// and skipping it keeps large carousels cheap to write.
func writeGalleryZip(zipWriter *zip.Writer, dir string, metadataJSON []byte, logWriter io.Writer) error {
	header := &zip.FileHeader{Name: galleryMetadataFilename, Method: zip.Deflate}
	header.SetModTime(time.Now())
	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create %s: %w", galleryMetadataFilename, err)
	}
	if _, err := writer.Write(metadataJSON); err != nil {
		return fmt.Errorf("write %s: %w", galleryMetadataFilename, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		method := zip.Deflate
		if !strings.HasSuffix(name, ".json") {
			method = zip.Store
		}
		if err := addGalleryFileToZip(zipWriter, dir, name, method); err != nil {
			fmt.Fprintf(logWriter, "Failed to add %s to ZIP: %v\n", name, err)
			return fmt.Errorf("add file %s: %w", name, err)
		}
	}
	return nil
}

func addGalleryFileToZip(zipWriter *zip.Writer, dir, name string, method uint16) error {
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header := &zip.FileHeader{Name: name, Method: method}
	header.SetModTime(info.ModTime())

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	// Raw extractor sidecars are sanitized BEFORE they are stored (G12): the
	// bundle is publicly downloadable, and Instagram/X sidecars carry signed
	// CDN URLs and session-derived fields. The /gallery/:id/raw endpoint
	// sanitizes on the way out too, but the copy at rest must already be safe.
	// Media bytes pass through untouched.
	if strings.HasSuffix(name, ".json") && name != galleryMetadataFilename {
		raw, err := io.ReadAll(io.LimitReader(file, 16<<20))
		if err != nil {
			return err
		}
		sanitized, sanitizeErr := SanitizeJSON(raw, utils.MediaProxyRedactionSecrets())
		if sanitizeErr != nil {
			// An unparseable sidecar still gets secret-string redaction rather
			// than being stored verbatim or dropped.
			sanitized = []byte(utils.RedactSecrets(string(raw), utils.MediaProxyRedactionSecrets()))
		}
		_, err = writer.Write(sanitized)
		return err
	}

	_, err = io.Copy(writer, file)
	return err
}
