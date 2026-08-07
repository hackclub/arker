package archivers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gorm.io/gorm"

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

// GalleryMetadata is Arker's normalized view of a gallery-dl capture. The
// raw, site-specific metadata is preserved alongside it in the ZIP, so this
// only needs to carry the fields a viewer wants: who posted, what they said,
// when, and what files came back.
type GalleryMetadata struct {
	SourceURL   string        `json:"source_url"`
	Extractor   string        `json:"extractor,omitempty"`
	Subcategory string        `json:"subcategory,omitempty"`
	PostID      string        `json:"post_id,omitempty"`
	PostURL     string        `json:"post_url,omitempty"`
	Author      string        `json:"author,omitempty"`
	AuthorName  string        `json:"author_name,omitempty"`
	Title       string        `json:"title,omitempty"`
	Description string        `json:"description,omitempty"`
	Date        string        `json:"date,omitempty"`
	Likes       *int64        `json:"likes,omitempty"`
	Tags        []string      `json:"tags,omitempty"`
	FileCount   int           `json:"file_count"`
	Files       []GalleryFile `json:"files"`
	ToolVersion string        `json:"gallery_dl_version,omitempty"`
	ArchivedAt  string        `json:"archived_at"`
}

// GalleryFile describes one downloaded media file inside the ZIP.
type GalleryFile struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type,omitempty"`
	IsVideo      bool   `json:"is_video"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
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

	cmd := exec.CommandContext(ctx, "gallery-dl")
	cmd.Args = append(cmd.Args, args...)
	cmd.Stdout = redactedLog
	cmd.Stderr = redactedLog
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

	// gallery-dl exits non-zero for partial failures too, so a run that still
	// produced media is worth keeping. Only fail when nothing came back.
	if len(media) == 0 {
		if runErr != nil {
			fmt.Fprintf(logWriter, "gallery-dl failed: %v (%s)\n", runErr, describeGalleryDlExit(runErr))
			return Result{}, fmt.Errorf("gallery-dl failed: %w (%s)", runErr, describeGalleryDlExit(runErr))
		}
		fmt.Fprintf(logWriter, "gallery-dl downloaded no files for %s\n", url)
		return Result{}, fmt.Errorf("gallery-dl downloaded no files for %s", url)
	}
	if runErr != nil {
		fmt.Fprintf(logWriter, "gallery-dl exited with %v (%s) but produced %d file(s); keeping partial archive\n",
			runErr, describeGalleryDlExit(runErr), len(media))
	}

	metadata := buildGalleryMetadata(tmpDir, url, version, media, sidecars, logWriter)
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

	return Result{Data: pipeReader, Extension: ".zip", ContentType: "application/zip"}, nil
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
		"--no-part",
		"-R", "3",
		"--http-timeout", "30",
		// Deliberately not --verbose. Default output already names every file
		// written and prints a one-line "[site][error] ..." on failure, which
		// is what the archive log and the failure pane need; verbose adds
		// urllib3 chatter and a full Python traceback around it.
	}
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
			}
		}
		meta.Files = append(meta.Files, file)
	}

	return meta
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
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	}
	if byExt := mime.TypeByExtension(filepath.Ext(name)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
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
	_, err = io.Copy(writer, file)
	return err
}
