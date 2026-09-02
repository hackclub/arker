package brightdata

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// SupportsStoredVideoMetadata reports whether Bright Data can resolve a post
// record without buying or replacing media bytes. Dataset-backed platforms
// need only the API key. YouTube needs a Browser API session, but only to call
// Innertube from an accepted network position; it never transfers the video.
func (c *Client) SupportsStoredVideoMetadata(targetURL string) bool {
	if !c.Enabled() {
		return false
	}
	switch {
	case utils.IsInstagramURL(targetURL), utils.IsTikTokURL(targetURL), utils.IsFacebookURL(targetURL):
		return true
	case utils.IsYouTubeURL(targetURL):
		return c.BrowserReady() && ExtractYouTubeVideoID(targetURL) != ""
	default:
		return false
	}
}

// RefreshStoredVideoMetadata resolves only structured metadata for historical
// video bytes Arker already holds. This is intentionally separate from
// ArchiveFallback: re-downloading hundreds of gigabytes merely to recover
// sidecars would be slower, costlier, and capable of silently changing the
// archived artifact. The worker probes the stored object after this returns,
// so byte-derived duration and dimensions remain authoritative.
func (c *Client) RefreshStoredVideoMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, media archivers.VideoMedia) (archivers.Result, error) {
	if !c.SupportsStoredVideoMetadata(targetURL) {
		return archivers.Result{}, fmt.Errorf("no metadata-only Bright Data path for %s", targetURL)
	}
	if logWriter == nil {
		logWriter = io.Discard
	}
	shortID := shortIDForItem(db, itemID)
	switch {
	case utils.IsInstagramURL(targetURL):
		return c.refreshStoredInstagramMetadata(ctx, targetURL, logWriter, db, itemID, shortID, media)
	case utils.IsTikTokURL(targetURL):
		return c.refreshStoredTikTokMetadata(ctx, targetURL, logWriter, db, itemID, shortID, media)
	case utils.IsFacebookURL(targetURL):
		return c.refreshStoredFacebookMetadata(ctx, targetURL, logWriter, db, itemID, shortID, media)
	case utils.IsYouTubeURL(targetURL):
		return c.refreshStoredYouTubeMetadata(ctx, targetURL, logWriter, db, itemID, shortID, media)
	default:
		return archivers.Result{}, fmt.Errorf("no metadata-only Bright Data path for %s", targetURL)
	}
}

func metadataOnlyResult(metadata, raw *archivers.Sidecar) archivers.Result {
	return archivers.Result{
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Source:       models.ArchiveSourceBrightData,
		Metadata:     metadata,
		RawMetadata:  raw,
		Completeness: archivers.CompletenessComplete,
	}
}

func (c *Client) refreshStoredInstagramMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string, media archivers.VideoMedia) (archivers.Result, error) {
	datasets := []string{DatasetInstagramPosts}
	if strings.Contains(strings.ToLower(targetURL), "/reel/") {
		datasets = []string{DatasetInstagramReels, DatasetInstagramPosts}
	}
	var lastErr error
	for _, datasetID := range datasets {
		usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
		record, err := c.resolveRecord(ctx, db, usage, datasetID, targetURL, logWriter)
		if err != nil {
			lastErr = err
			continue
		}
		metadata, raw, err := buildBrightDataInstagramVideoArtifacts(record, targetURL, media.SizeBytes, time.Now())
		if err != nil {
			usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
			c.recordUsage(db, usage)
			lastErr = err
			continue
		}
		usage.Success = true
		usage.Detail = "metadata-only historical repair"
		c.recordUsage(db, usage)
		return metadataOnlyResult(metadata, raw), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Instagram dataset could resolve %s", targetURL)
	}
	return archivers.Result{}, lastErr
}

func (c *Client) refreshStoredTikTokMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string, media archivers.VideoMedia) (archivers.Result, error) {
	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetTikTokPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	metadata, raw, err := buildBrightDataTikTokVideoArtifacts(record, targetURL, media.SizeBytes, time.Now())
	if err != nil {
		usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	usage.Success = true
	usage.Detail = "metadata-only historical repair"
	c.recordUsage(db, usage)
	return metadataOnlyResult(metadata, raw), nil
}

func (c *Client) refreshStoredFacebookMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string, media archivers.VideoMedia) (archivers.Result, error) {
	usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, usage, DatasetFacebookPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	videoURL := firstFacebookVideo(facebookMediaEntries(record, logWriter))
	metadata, raw, err := buildBrightDataFacebookVideoArtifacts(record, targetURL, videoURL, media.SizeBytes, time.Now())
	if err != nil {
		usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	usage.Success = true
	usage.Detail = "metadata-only historical repair"
	c.recordUsage(db, usage)
	return metadataOnlyResult(metadata, raw), nil
}

func (c *Client) refreshStoredYouTubeMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string, media archivers.VideoMedia) (archivers.Result, error) {
	videoID := ExtractYouTubeVideoID(targetURL)
	if videoID == "" {
		return archivers.Result{}, fmt.Errorf("could not extract a YouTube video ID from %s", targetURL)
	}
	watchURL := "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
	usage := &models.BrightDataUsage{
		ArchiveItemID: itemID,
		ShortID:       shortID,
		URL:           targetURL,
		Product:       "browser_api",
	}
	sessions := 0
	finishUsage := func(success bool, detail string) {
		usage.BytesTransferred, usage.CostUSD = c.browserSessionCost(0, sessions)
		usage.Success = success
		usage.Detail = truncate(detail, 500)
		c.recordUsage(db, usage)
	}

	var lastErr error
	for _, country := range []string{"", "de", "gb"} {
		if lastErr != nil {
			fmt.Fprintf(logWriter, "Retrying metadata resolution with a different session geography (%s) after: %v\n", countryLabel(country), lastErr)
		}
		session, err := c.openBrowserSession(ctx, country, watchURL, logWriter)
		if err != nil {
			if sessionConnected(err) {
				sessions++
			}
			lastErr = err
		} else {
			sessions++
			info, resolveErr := resolveYouTubeMedia(session, videoID, c.cfg)
			session.Close()
			if resolveErr == nil {
				// Delivery facts describe the provider's current progressive
				// stream, not necessarily the historical bytes. Keep post facts
				// and let the worker's stored-object probe fill media facts.
				info.FormatID = ""
				info.QualityLabel = ""
				info.Width, info.Height, info.FPS = 0, 0, 0
				info.MimeType = "video/mp4"
				metadata, raw, buildErr := buildBrightDataYouTubeArtifacts(info, targetURL, videoID, media.SizeBytes, time.Now())
				if buildErr != nil {
					finishUsage(false, "metadata build failed: "+buildErr.Error())
					return archivers.Result{}, buildErr
				}
				finishUsage(true, "metadata-only historical repair")
				return metadataOnlyResult(metadata, raw), nil
			}
			lastErr = resolveErr
		}
		if ctx.Err() != nil || !retryableInAnotherCountry(lastErr) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("YouTube metadata resolution returned no result")
	}
	finishUsage(false, lastErr.Error())
	return archivers.Result{}, lastErr
}
