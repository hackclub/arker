package apify

import (
	"context"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// resolveRecord runs an actor for one post and returns the first usable
// dataset item. Error records — the way every vetted actor reports a missing
// or private post — fail here with errNotFound so the platform flow can stop
// instead of retrying a post that is gone.
func (c *Client) resolveRecord(ctx context.Context, db *gorm.DB, usage *models.FallbackUsage, actorID string, input any, logWriter io.Writer, idKeys ...string) (map[string]any, *run, error) {
	finished, err := c.runActor(ctx, db, usage, actorID, input, logWriter)
	if err != nil {
		return nil, nil, err
	}
	for _, record := range finished.Items {
		if err := recordError(record, idKeys...); err != nil {
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return nil, finished, err
		}
		return record, finished, nil
	}
	err = fmt.Errorf("%w: actor returned no items for %s", errNotFound, usage.URL)
	usage.Detail = truncate(err.Error(), 500)
	c.recordUsage(db, usage)
	return nil, finished, err
}

// videoPlan is everything a platform flow has decided about a video before
// the bytes move: where to fetch it, what to call it, and how to describe it.
type videoPlan struct {
	// URL is the MP4 to download: a platform CDN URL or an actor key-value
	// store record.
	URL string
	// Metadata is the normalized description, complete except for the media
	// size and the archived-at stamp, which are only known after download.
	Metadata *archivers.VideoMetadata
	// Record is the raw actor item, stored sanitized beside the metadata.
	Record map[string]any
	// ThumbnailURL is the platform's poster; optional.
	ThumbnailURL string
	// TempPattern names the temp file (for log readability).
	TempPattern string
}

// archiveVideo downloads a plan's MP4, verifies it, builds both sidecars and
// finalizes the usage row. It is the one place every yt-dlp-type fallback
// funnels through so the artifact contract is met identically per platform.
func (c *Client) archiveVideo(ctx context.Context, plan videoPlan, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	if plan.URL == "" {
		err := fmt.Errorf("actor record for %s has no downloadable video", usage.URL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	pattern := plan.TempPattern
	if pattern == "" {
		pattern = "arker-apify-video-*.mp4"
	}
	if isApifyHost(plan.URL) {
		fmt.Fprintf(logWriter, "Downloading video from the actor's key-value store...\n")
	} else {
		fmt.Fprintf(logWriter, "Downloading video from the platform CDN...\n")
	}
	videoPath, size, err := c.downloadToTemp(ctx, plan.URL, pattern)
	if err != nil {
		err = fmt.Errorf("video download failed: %w", err)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	if err := verifyMP4(videoPath); err != nil {
		removeFile(videoPath)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	if isApifyHost(plan.URL) {
		usage.BytesTransferred += size
	}
	fmt.Fprintf(logWriter, "Downloaded %d bytes of video via Apify fallback\n", size)

	// The record's own duration and dimensions are hints; the bytes are the
	// fact. Probe them now, while they are still a file rather than a stored
	// object, the way the native yt-dlp flow's ffprobe pass does.
	meta := plan.Metadata
	meta.Media.SizeBytes = size
	meta.Media.Extension = ".mp4"
	meta.Media.ContentType = "video/mp4"
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if probe, err := archivers.ProbeVideoFile(probeCtx, videoPath); err == nil {
		if probe.DurationSeconds != nil {
			meta.DurationSeconds = probe.DurationSeconds
		}
		if probe.Width != nil && probe.Height != nil {
			meta.Media.Width, meta.Media.Height = probe.Width, probe.Height
		}
	} else {
		fmt.Fprintf(logWriter, "ffprobe skipped: %v\n", err)
	}
	cancel()
	meta.ArchivedAt = time.Now().UTC().Format(time.RFC3339)
	meta.SchemaVersion = archivers.VideoMetadataSchemaVersion
	meta.Provenance = models.ArchiveSourceApify
	if meta.Provider == "" {
		meta.Provider = "apify:" + usage.Product
	}

	metadataJSON, err := archivers.MarshalVideoMetadata(meta)
	if err != nil {
		removeFile(videoPath)
		usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, fmt.Errorf("failed to build video metadata: %w", err)
	}
	raw, err := rawSidecar(plan.Record)
	if err != nil {
		removeFile(videoPath)
		usage.Detail = truncate("record sanitize failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	reader, err := openTempFileReader(videoPath)
	if err != nil {
		removeFile(videoPath)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	usage.Success = true
	usage.Detail = fmt.Sprintf("video %d bytes", size)
	c.recordUsage(db, usage)

	return archivers.Result{
		Data:        reader,
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Thumbnail:   c.thumbnailFromURL(ctx, plan.ThumbnailURL, logWriter),
		Source:      models.ArchiveSourceApify,
		Metadata:    &archivers.Sidecar{Data: metadataJSON},
		RawMetadata: raw,
		// One post is one video: the muxed MP4 and both sidecars are stored,
		// so there is no second asset this could have missed.
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// metadataOnlyVideoArtifacts builds the sidecars for a stored-video metadata
// refresh, where the bytes already exist and only the description is rebuilt.
func metadataOnlyVideoArtifacts(meta *archivers.VideoMetadata, record map[string]any, media archivers.VideoMedia, product string) (*archivers.Sidecar, *archivers.Sidecar, error) {
	meta.Media = media
	meta.ArchivedAt = time.Now().UTC().Format(time.RFC3339)
	meta.SchemaVersion = archivers.VideoMetadataSchemaVersion
	meta.Provenance = models.ArchiveSourceApify
	if meta.Provider == "" {
		meta.Provider = "apify:" + product
	}
	metadataJSON, err := archivers.MarshalVideoMetadata(meta)
	if err != nil {
		return nil, nil, err
	}
	raw, err := rawSidecar(record)
	if err != nil {
		return nil, nil, err
	}
	return &archivers.Sidecar{Data: metadataJSON}, raw, nil
}

// galleryPlan is what a platform flow resolved for a gallery-dl-type post.
type galleryPlan struct {
	Entries []mediaEntry
	Audio   *galleryAudio
	Meta    *archivers.GalleryMetadata
	Record  map[string]any
	// PosterURL is used when no still image was stored to preview from.
	PosterURL string
	// Fetch overrides the fetcher; nil means Client.directFetch.
	Fetch mediaFetcher
}

// archiveGallery runs the shared gallery bundle build for a plan and
// finalizes the usage row.
func (c *Client) archiveGallery(ctx context.Context, plan galleryPlan, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	fetch := plan.Fetch
	if fetch == nil {
		fetch = c.directFetch
	}
	result, _, totalBytes, err := c.buildGalleryArchive(ctx, plan.Entries, plan.Audio, plan.Meta, plan.Record, fetch, logWriter)
	if err != nil {
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	if result.Thumbnail == nil && plan.PosterURL != "" {
		result.Thumbnail = c.thumbnailFromURL(ctx, plan.PosterURL, logWriter)
	}
	usage.Success = true
	usage.Detail = fmt.Sprintf("gallery %d file(s), %d bytes", len(plan.Meta.Files), totalBytes)
	c.recordUsage(db, usage)
	return result, nil
}

// kvsFetch downloads gallery entries, counting key-value-store bytes against
// the usage row so provider-side transfer is visible in spend accounting.
func (c *Client) kvsCountingFetch(usage *models.FallbackUsage) mediaFetcher {
	return func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		size, err := c.directFetch(ctx, entry, dest)
		if err == nil && isApifyHost(entry.URL) {
			usage.BytesTransferred += size
		}
		return size, err
	}
}
