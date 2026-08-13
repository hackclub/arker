package canary

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// Probe stages, in the order a probe passes through them. StageReached is the
// last one that succeeded; FailureStage is the one that did not.
const (
	StageRouting       = "routing"
	StageCapture       = "capture"
	StageArchive       = "archive"
	StageItemCompleted = "item_completed"
	StageMedia         = "media"
	StageMetadata      = "metadata"
	StageRawMetadata   = "raw_metadata"
	StageProvenance    = "provenance"
	StagePassed        = "passed"
)

// maxSidecarBytes bounds how much of a metadata sidecar the validator will
// read. Matches the API's own ceiling for stored video metadata.
const maxSidecarBytes = 32 * 1024 * 1024

// maxBufferedZipBytes bounds buffering a gallery ZIP into memory when the
// storage backend has no seekable reader. Real gallery posts are far smaller;
// anything larger is not worth an OOM to validate.
const maxBufferedZipBytes = 256 * 1024 * 1024

// Spend is the Bright Data usage attributed to a probe's capture.
type Spend struct {
	Operations int64
	CostUSD    float64
}

// Validation is the verdict on one probe.
type Validation struct {
	Passed        bool
	StageReached  string
	FailureStage  string
	FailureReason string
	MediaBytes    int64
	MediaCount    int
	ContentType   string
	Provenance    string
	// Warnings are observations that are suspicious but not contract
	// violations. They are logged and stored in the failure reason only when
	// the probe otherwise passes, so a canary never goes red on a hunch.
	Warnings []string
}

func fail(stageReached, failureStage, format string, args ...any) Validation {
	return Validation{StageReached: stageReached, FailureStage: failureStage, FailureReason: fmt.Sprintf(format, args...)}
}

// ValidateRouting checks that the probe URL still routes to the archive type
// the probe expects, before anything is downloaded.
//
// This is a real contract check, not a precondition: "recognized social post
// reaches its extractor" is the routing half of the contract, and a URL that
// quietly stopped being recognized would otherwise produce a green
// MHTML-and-screenshot capture — exactly the silent non-social success the
// contract forbids.
func ValidateRouting(probe Probe) Validation {
	types := utils.GetArchiveTypes(probe.URL)
	for _, typ := range types {
		if utils.ArchiveTypesEqual(typ, probe.ExpectedType) {
			return Validation{Passed: true, StageReached: StageRouting}
		}
	}
	return fail("", StageRouting,
		"URL no longer routes to %s (routed to: %s); a recognized social post that loses its extractor would archive as page-only",
		probe.ExpectedType, strings.Join(types, ", "))
}

// ValidateArchive checks the full contract on a finished probe archive: the
// item completed, real media bytes of the expected kind are in storage,
// normalized metadata is present and sane, the raw provider record is
// retrievable, and provenance is the free native path.
//
// It is a pure function of (probe, item, storage contents, spend), so the
// interesting failure modes are all reachable offline from fixtures.
func ValidateArchive(probe Probe, item *models.ArchiveItem, store storage.Storage, spend Spend, paidAllowed bool) Validation {
	if item == nil {
		return fail(StageArchive, StageItemCompleted, "no archive item was found for the probe capture")
	}

	provenance := item.Source
	if provenance == "" {
		provenance = models.ArchiveSourceNative
	}

	if item.Status != "completed" {
		v := fail(StageArchive, StageItemCompleted, "archive item is %q, not completed", item.Status)
		v.Provenance = provenance
		return v
	}
	if item.StorageKey == "" {
		v := fail(StageArchive, StageItemCompleted, "archive item is completed but has no storage key")
		v.Provenance = provenance
		return v
	}

	var v Validation
	switch {
	case utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp):
		v = validateVideoArchive(probe, item, store)
	case utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeGalleryDl):
		v = validateGalleryArchive(probe, item, store)
	default:
		v = fail(StageItemCompleted, StageMedia, "probe produced an unexpected archive type %q", item.Type)
	}
	v.Provenance = provenance
	if !v.Passed {
		return v
	}

	// Provenance last, and deliberately strict. A canary runs with no paid
	// archiver in its map, so a brightdata artifact or a billed operation here
	// means the guard was bypassed — that is a louder problem than a broken
	// extractor and must never read green.
	if provenance != models.ArchiveSourceNative {
		return withProvenance(fail(StageRawMetadata, StageProvenance,
			"artifact came from %q, not the native path; canaries must never trigger paid fallback", provenance), provenance)
	}
	if !paidAllowed && (spend.Operations > 0 || spend.CostUSD > 0) {
		return withProvenance(fail(StageRawMetadata, StageProvenance,
			"%d paid Bright Data operation(s) costing $%.4f were recorded against this capture; canaries must never spend money",
			spend.Operations, spend.CostUSD), provenance)
	}

	v.Passed = true
	v.StageReached = StagePassed
	return v
}

func withProvenance(v Validation, provenance string) Validation {
	v.Provenance = provenance
	return v
}

// validateVideoArchive checks a yt-dlp probe result.
func validateVideoArchive(probe Probe, item *models.ArchiveItem, store storage.Storage) Validation {
	size, err := store.Size(item.StorageKey)
	if err != nil {
		return fail(StageItemCompleted, StageMedia, "stored media is unreadable at %q: %v", item.StorageKey, err)
	}
	v := Validation{MediaBytes: size, MediaCount: 1}
	if size < probe.MinMediaBytes {
		return fail(StageItemCompleted, StageMedia,
			"stored media is %d bytes, below the %d-byte floor for this probe (a completed item with no real asset is a false green)", size, probe.MinMediaBytes)
	}
	if item.FileSize != size {
		v.Warnings = append(v.Warnings, fmt.Sprintf("archive_items.file_size is %d but storage holds %d bytes", item.FileSize, size))
	}

	if item.MetadataKey == "" {
		v.FailureStage, v.FailureReason, v.StageReached = StageMetadata, "normalized metadata sidecar is missing (metadata_key is empty)", StageMedia
		return v
	}
	raw, err := readJSON(store, item.MetadataKey)
	if err != nil {
		v.FailureStage, v.FailureReason, v.StageReached = StageMetadata, fmt.Sprintf("normalized metadata is unreadable: %v", err), StageMedia
		return v
	}
	var meta archivers.VideoMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		v.FailureStage, v.FailureReason, v.StageReached = StageMetadata, fmt.Sprintf("normalized metadata does not parse as video metadata: %v", err), StageMedia
		return v
	}
	v.ContentType = meta.Media.ContentType
	if reason := videoMetadataProblem(meta); reason != "" {
		v.FailureStage, v.FailureReason, v.StageReached = StageMetadata, reason, StageMedia
		return v
	}
	if probe.ExpectedMedia == MediaKindVideo && !strings.HasPrefix(meta.Media.ContentType, "video/") {
		v.FailureStage, v.FailureReason, v.StageReached = StageMedia, fmt.Sprintf("stored media content type is %q, expected a video/* asset", meta.Media.ContentType), StageItemCompleted
		return v
	}
	if meta.Media.SizeBytes > 0 && !withinTolerance(meta.Media.SizeBytes, size, 0.05) {
		v.Warnings = append(v.Warnings, fmt.Sprintf("metadata reports %d media bytes but storage holds %d", meta.Media.SizeBytes, size))
	}

	if item.RawMetadataKey == "" {
		v.FailureStage, v.FailureReason, v.StageReached = StageRawMetadata, "raw provider metadata is missing (raw_metadata_key is empty)", StageMetadata
		return v
	}
	rawProvider, err := readJSON(store, item.RawMetadataKey)
	if err != nil {
		v.FailureStage, v.FailureReason, v.StageReached = StageRawMetadata, fmt.Sprintf("raw provider metadata is unretrievable: %v", err), StageMetadata
		return v
	}
	if len(rawProvider) < 2 {
		v.FailureStage, v.FailureReason, v.StageReached = StageRawMetadata, "raw provider metadata is empty", StageMetadata
		return v
	}

	v.Passed = true
	v.StageReached = StageRawMetadata
	return v
}

// videoMetadataProblem returns a reason string when normalized video metadata
// is present but not credible, or "" when it is sane.
//
// "Sane" is deliberately shallow: post identity, a title or description, a
// declared media content type, and a parseable archive timestamp. Deeper
// assertions (exact durations, engagement counts) would turn ordinary platform
// churn into pages.
func videoMetadataProblem(meta archivers.VideoMetadata) string {
	if meta.SchemaVersion == "" {
		return "normalized metadata has no schema_version"
	}
	if meta.PostID == "" && meta.CanonicalURL == "" {
		return "normalized metadata identifies no post (both post_id and canonical_url are empty)"
	}
	if meta.Title == "" && meta.Description == "" {
		return "normalized metadata has neither a title nor a description"
	}
	if meta.Media.ContentType == "" {
		return "normalized metadata declares no media content type"
	}
	if meta.ArchivedAt == "" {
		return "normalized metadata has no archived_at timestamp"
	}
	if _, err := time.Parse(time.RFC3339, meta.ArchivedAt); err != nil {
		return fmt.Sprintf("normalized metadata archived_at %q is not RFC3339", meta.ArchivedAt)
	}
	return ""
}

// validateGalleryArchive checks a gallery-dl probe result: the ZIP opens, it
// holds real media files, Arker's normalized metadata.json is present and
// consistent with what is actually inside, and at least one raw gallery-dl
// sidecar survived.
func validateGalleryArchive(probe Probe, item *models.ArchiveItem, store storage.Storage) Validation {
	zipReader, cleanup, err := openZip(store, item.StorageKey)
	if err != nil {
		return fail(StageItemCompleted, StageMedia, "stored gallery bundle is unreadable: %v", err)
	}
	defer cleanup()

	var (
		v            Validation
		mediaBytes   int64
		mediaCount   int
		imageCount   int
		videoCount   int
		rawSidecars  int
		firstType    string
		metaFileSeen bool
		meta         archivers.GalleryMetadata
	)
	for _, f := range zipReader.File {
		if f.Name == "metadata.json" {
			metaFileSeen = true
			if r, openErr := f.Open(); openErr == nil {
				_ = json.NewDecoder(r).Decode(&meta)
				r.Close()
			}
			continue
		}
		if strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			rawSidecars++
			continue
		}
		contentType := galleryContentType(f.Name)
		switch {
		case strings.HasPrefix(contentType, "image/"):
			imageCount++
		case strings.HasPrefix(contentType, "video/"):
			videoCount++
		default:
			continue
		}
		if firstType == "" {
			firstType = contentType
		}
		mediaCount++
		mediaBytes += int64(f.UncompressedSize64)
	}
	v.MediaBytes, v.MediaCount, v.ContentType = mediaBytes, mediaCount, firstType

	if mediaCount == 0 {
		return fail(StageItemCompleted, StageMedia, "gallery bundle contains no image or video files")
	}
	if mediaBytes < probe.MinMediaBytes {
		return fail(StageItemCompleted, StageMedia,
			"gallery bundle holds %d media bytes, below the %d-byte floor for this probe", mediaBytes, probe.MinMediaBytes)
	}
	// The partial-download check that does not trust the extractor's own
	// accounting: the probe knows how many assets this post has, so a bundle
	// with fewer is a partial archive no matter how self-consistent its
	// metadata looks.
	if probe.MinMediaCount > 0 && mediaCount < probe.MinMediaCount {
		return fail(StageItemCompleted, StageMedia,
			"gallery bundle holds %d media files but this post has %d (partial download reading as complete)",
			mediaCount, probe.MinMediaCount)
	}
	if probe.ExpectedMedia == MediaKindImage && imageCount == 0 {
		return fail(StageItemCompleted, StageMedia, "gallery bundle has %d video file(s) but no images, and this probe expects images", videoCount)
	}
	if probe.ExpectedMedia == MediaKindVideo && videoCount == 0 {
		return fail(StageItemCompleted, StageMedia, "gallery bundle has %d image(s) but no video, and this probe expects video", imageCount)
	}

	if !metaFileSeen {
		v.StageReached, v.FailureStage, v.FailureReason = StageMedia, StageMetadata, "gallery bundle has no normalized metadata.json"
		return v
	}
	if reason := galleryMetadataProblem(meta, mediaCount); reason != "" {
		v.StageReached, v.FailureStage, v.FailureReason = StageMedia, StageMetadata, reason
		return v
	}
	if rawSidecars == 0 {
		v.StageReached, v.FailureStage, v.FailureReason = StageMetadata, StageRawMetadata, "gallery bundle carries no raw gallery-dl metadata sidecars"
		return v
	}

	v.Passed = true
	v.StageReached = StageRawMetadata
	return v
}

// galleryMetadataProblem checks normalized gallery metadata against the ZIP's
// actual contents.
//
// The file-count comparison catches metadata that describes more files than
// the bundle holds. Today gallery-dl's file_count records what it managed to
// download rather than what the post contains, so this fires only on genuine
// metadata/bundle disagreement — the "3 of 10 slides" case is caught by the
// probe's own MinMediaCount instead, and this check gets stronger for free if
// expected-count tracking lands upstream in the archiver.
func galleryMetadataProblem(meta archivers.GalleryMetadata, mediaCount int) string {
	if meta.PostID == "" && meta.PostURL == "" {
		return "gallery metadata identifies no post (both post_id and post_url are empty)"
	}
	if meta.Extractor == "" {
		return "gallery metadata names no extractor"
	}
	if meta.ArchivedAt == "" {
		return "gallery metadata has no archived_at timestamp"
	}
	if _, err := time.Parse(time.RFC3339, meta.ArchivedAt); err != nil {
		return fmt.Sprintf("gallery metadata archived_at %q is not RFC3339", meta.ArchivedAt)
	}
	if meta.FileCount > mediaCount {
		return fmt.Sprintf("gallery metadata claims %d files but the bundle holds %d media files (partial download)", meta.FileCount, mediaCount)
	}
	if len(meta.Files) > mediaCount {
		return fmt.Sprintf("gallery metadata lists %d files but the bundle holds %d media files (partial download)", len(meta.Files), mediaCount)
	}
	return ""
}

func withinTolerance(a, b int64, fraction float64) bool {
	if a == b {
		return true
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	larger := a
	if b > larger {
		larger = b
	}
	if larger <= 0 {
		return false
	}
	return float64(diff)/float64(larger) <= fraction
}

func readJSON(store storage.Storage, key string) ([]byte, error) {
	reader, err := store.Reader(key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxSidecarBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSidecarBytes {
		return nil, fmt.Errorf("metadata exceeds %d bytes", maxSidecarBytes)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("stored metadata is not valid JSON")
	}
	return data, nil
}

type seekerReaderAt struct{ seeker storage.ReadSeekCloser }

func (s *seekerReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := s.seeker.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(s.seeker, p)
}

// openZip opens a stored ZIP, preferring a seekable reader and falling back to
// buffering, mirroring how the gallery viewer reads the same objects.
func openZip(store storage.Storage, key string) (*zip.Reader, func(), error) {
	size, err := store.Size(key)
	if err != nil {
		return nil, nil, err
	}
	if seekable, ok := store.(storage.SeekableStorage); ok {
		reader, err := seekable.SeekableReader(key)
		if err == nil {
			zr, zerr := zip.NewReader(&seekerReaderAt{seeker: reader}, size)
			if zerr == nil {
				return zr, func() { _ = reader.Close() }, nil
			}
			_ = reader.Close()
			return nil, nil, zerr
		}
	}
	if size > maxBufferedZipBytes {
		return nil, nil, fmt.Errorf("gallery bundle is %d bytes, too large to validate without a seekable reader", size)
	}
	reader, err := store.Reader(key)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	zr, err := zip.NewReader(newBytesReaderAt(data), int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	return zr, func() {}, nil
}

type bytesReaderAt struct{ data []byte }

func newBytesReaderAt(data []byte) *bytesReaderAt { return &bytesReaderAt{data: data} }

func (b *bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// galleryContentType maps a bundled filename to a content type, matching the
// gallery viewer's own mapping.
func galleryContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
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
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
